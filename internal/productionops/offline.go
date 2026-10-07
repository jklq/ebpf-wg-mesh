package productionops

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"ebof-wg-mesh/internal/controlplane/secretkeys"
	"ebof-wg-mesh/internal/controlplane/signkeys"
	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/reconciliation"
	"ebof-wg-mesh/internal/recovery"
	"go.yaml.in/yaml/v3"
)

type offlineInput struct {
	Config          recovery.Config   `json:"config"`
	Point           recovery.Point    `json:"point"`
	Files           map[string]string `json:"files"`
	Workspace       string            `json:"workspace"`
	ParentNetworkNS string            `json:"parentNetworkNS"`
}

// offlineRecovery is the packaged implementation of the native drill protocol.
// Every executable comes from the selected protected release. All descendants
// live under the caller's fresh PID/network namespace and die with that namespace.
func offlineRecovery(ctx context.Context, workspace, inputPath string) (recovery.DrillReady, error) {
	var ready recovery.DrillReady
	var input offlineInput
	if err := privateJSON(inputPath, &input); err != nil {
		return ready, err
	}
	current, err := os.Readlink("/proc/self/ns/net")
	if err != nil || current == input.ParentNetworkNS || workspace != input.Workspace {
		return ready, fmt.Errorf("offline recovery requires the fresh native recovery namespace")
	}
	interfaces, err := net.Interfaces()
	if err != nil || len(interfaces) != 1 || interfaces[0].Name != "lo" {
		return ready, fmt.Errorf("offline recovery must have only loopback")
	}
	var release deploy.Release
	var installation deploy.Installation
	for _, d := range input.Point.Dependencies {
		switch d.Kind {
		case "release":
			if err := privateJSON(input.Files[d.Kind+"/"+d.ID], &release); err != nil {
				return ready, err
			}
		case "installation":
			if err := privateJSON(input.Files[d.Kind+"/"+d.ID], &installation); err != nil {
				return ready, err
			}
		}
	}
	if release.Schema != input.Point.Snapshot.Schema || release.ConsoleSchema != input.Point.Snapshot.ConsoleSchema {
		return ready, fmt.Errorf("offline release differs from selected schemas")
	}
	executable := func(kind, name string) (string, error) {
		path := input.Files["tool/"+release.ID+"/"+kind+"/"+name+"/"+runtime.GOARCH]
		if path == "" {
			return "", fmt.Errorf("protected %s %s executable is missing", kind, name)
		}
		return path, nil
	}
	start := func(name, binary string, args []string, environment map[string]string) error {
		log, err := os.OpenFile(filepath.Join(workspace, name+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer log.Close()
		cmd := exec.Command(binary, args...)
		cmd.Stdout, cmd.Stderr = log, log
		cmd.Dir = workspace
		cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C"}
		for key, value := range environment {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
		if err := cmd.Start(); err != nil {
			return err
		}
		return saveJSON(filepath.Join(workspace, name+".pid"), cmd.Process.Pid)
	}
	ca, err := certificate("offline-db-ca", nil, nil)
	if err != nil {
		return ready, err
	}
	node, err := certificate("node", &ca, []string{"127.0.0.1", "localhost"})
	if err != nil {
		return ready, err
	}
	root, err := certificate("root", &ca, nil)
	if err != nil {
		return ready, err
	}
	certDir := filepath.Join(workspace, "certs")
	for name, b := range map[string][]byte{"ca.crt": ca.Certificate, "node.crt": node.Certificate, "node.key": node.Key, "client.root.crt": root.Certificate, "client.root.key": root.Key} {
		if err := writePrivate(filepath.Join(certDir, name), b); err != nil {
			return ready, err
		}
	}
	cockroach, err := executable("program", "cockroachdb")
	if err != nil {
		return ready, err
	}
	if err := start("database", cockroach, []string{"start-single-node", "--certs-dir=" + certDir, "--store=" + filepath.Join(workspace, "store"), "--listen-addr=127.0.0.1:26257", "--http-addr=127.0.0.1:8080", "--external-io-dir=" + filepath.Join(workspace, "database"), "--cache=128MiB", "--max-sql-memory=128MiB"}, nil); err != nil {
		return ready, err
	}
	url := fmt.Sprintf("postgresql://root@127.0.0.1:26257/defaultdb?sslmode=verify-full&sslrootcert=%s&sslcert=%s&sslkey=%s", certDir+"/ca.crt", certDir+"/client.root.crt", certDir+"/client.root.key")
	db, err := sql.Open("pgx", url)
	if err != nil {
		return ready, err
	}
	defer db.Close()
	deadline := time.Now().Add(60 * time.Second)
	for db.PingContext(ctx) != nil {
		if time.Now().After(deadline) {
			return ready, fmt.Errorf("offline database startup did not converge")
		}
		select {
		case <-ctx.Done():
			return ready, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	if err := recovery.CheckEmptyDestination(ctx, db); err != nil {
		return ready, err
	}
	collection := "nodelocal://1/" + strings.Trim(input.Config.BackupPrefix, "/")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	if _, err := db.ExecContext(ctx, "RESTORE FROM "+quote(input.Point.Database.Subdirectory)+" IN "+quote(collection)+" AS OF SYSTEM TIME "+quote(input.Point.Snapshot.Timestamp.UTC().Format(time.RFC3339Nano))); err != nil {
		return ready, err
	}
	var databaseName string
	for _, query := range []string{`SELECT table_catalog FROM information_schema.tables WHERE table_name='recovery_runtime_authority' LIMIT 1`, `SELECT database_name FROM [SHOW DATABASES] WHERE database_name NOT IN ('system','defaultdb','postgres','platform_recovery') LIMIT 1`} {
		if err = db.QueryRowContext(ctx, query).Scan(&databaseName); err == nil {
			break
		}
	}
	if databaseName == "" {
		return ready, fmt.Errorf("restored platform database is missing")
	}
	url = strings.Replace(url, "/defaultdb?", "/"+databaseName+"?", 1)
	db.Close()
	db, err = sql.Open("pgx", url)
	if err != nil {
		return ready, err
	}
	defer db.Close()
	ring := struct {
		Version int                        `json:"version"`
		Keys    map[string]json.RawMessage `json:"keys"`
	}{1, map[string]json.RawMessage{}}
	for _, d := range input.Point.Dependencies {
		if d.Kind == "keyring" {
			var part struct {
				Keys map[string]json.RawMessage `json:"keys"`
			}
			if err := privateJSON(input.Files["keyring/"+d.ID], &part); err != nil {
				return ready, err
			}
			key, ok := part.Keys[d.ID]
			if !ok {
				return ready, fmt.Errorf("protected key version is missing")
			}
			ring.Keys[d.ID] = key
		}
		if d.Kind == "console-key" {
			ready.ConsoleKeyFiles = append(ready.ConsoleKeyFiles, input.Files["console-key/"+d.ID])
		}
	}
	ready.KeyringFile = filepath.Join(workspace, "keyring.json")
	if err := saveJSON(ready.KeyringFile, ring); err != nil {
		return ready, err
	}
	// Validate all original identities and decrypt every ciphertext before starting
	// any runtime process. Offline smoke restores preserve those original identities.
	if err := recovery.CheckRestoredDatabase(ctx, db, input.Point, input.Config.ConsoleSchema, ready.KeyringFile, ready.ConsoleKeyFiles); err != nil {
		return ready, err
	}
	provider, err := secretkeys.NewKeyring(ready.KeyringFile, secretkeys.KeyringOptions{})
	if err != nil {
		return ready, err
	}
	keys := secretkeys.New(db, provider)
	defer keys.Close()
	signing := signkeys.New(db, keys.Registry())
	oldCA, err := signing.PublicBundle(ctx, signkeys.ScopeInternalCA)
	if err != nil {
		return ready, err
	}
	var authority reconciliation.Authority
	if err := db.QueryRowContext(ctx, `SELECT installation,generation FROM recovery_runtime_authority WHERE singleton=TRUE`).Scan(&authority.InstallationID, &authority.Generation); err != nil {
		return ready, err
	}
	authority.ClusterID, authority.Paused = clusterID(oldCA), true
	authorityFile := filepath.Join(workspace, "authority.json")
	if err := saveJSON(authorityFile, authority); err != nil {
		return ready, err
	}
	ready.SourceDirectory = filepath.Join(workspace, "sources")
	for _, d := range input.Point.Dependencies {
		if d.Kind != "source" {
			continue
		}
		if filepath.IsAbs(d.ID) || filepath.Clean(d.ID) != d.ID || strings.Contains(d.ID, "..") {
			return ready, fmt.Errorf("invalid protected source path")
		}
		b, err := os.ReadFile(input.Files["source/"+d.ID])
		if err != nil {
			return ready, err
		}
		if err := writePrivate(filepath.Join(ready.SourceDirectory, d.ID), b); err != nil {
			return ready, err
		}
	}
	if err := os.MkdirAll(ready.SourceDirectory, 0700); err != nil {
		return ready, err
	}
	cp, err := executable("program", "controlplane")
	if err != nil {
		return ready, err
	}
	env := map[string]string{"CONTROLPLANE_PROFILE": "production", "CONTROLPLANE_DB_URL": url, "CONTROLPLANE_SECRET_KEYS_KEYRING": ready.KeyringFile, "CONTROLPLANE_AUTHORITY_FILE": authorityFile, "CONTROLPLANE_STATE_DIR": workspace + "/controlplane", "CONTROLPLANE_HEALTH_LISTEN": "127.0.0.1:9090", "CONTROLPLANE_INTERNAL_LISTEN": "127.0.0.1:9443", "CONTROLPLANE_INTERNAL_SERVER_NAMES": "controlplane,127.0.0.1", "CONTROLPLANE_SOURCE_ARCHIVES_DIR": ready.SourceDirectory, "CONTROLPLANE_INGRESS_PLATFORM_TLS_CERT_FILE": certDir + "/node.crt", "CONTROLPLANE_INGRESS_PLATFORM_TLS_KEY_FILE": certDir + "/node.key", "CONTROLPLANE_INGRESS_PUBLIC_ADDR": "localhost"}
	if err := start("controlplane", cp, nil, env); err != nil {
		return ready, err
	}
	registry, err := executable("program", "registry")
	if err != nil {
		return ready, err
	}
	registryConfig := workspace + "/registry.yaml"
	registryDocument, err := yaml.Marshal(map[string]any{"version": 0.1, "storage": map[string]any{"filesystem": map[string]any{"rootdirectory": workspace + "/registry"}}, "http": map[string]any{"addr": "127.0.0.1:5443", "tls": map[string]any{"certificate": certDir + "/node.crt", "key": certDir + "/node.key"}}})
	if err != nil {
		return ready, err
	}
	if err := writePrivate(registryConfig, registryDocument); err != nil {
		return ready, err
	}
	if err := start("registry", registry, []string{"serve", registryConfig}, nil); err != nil {
		return ready, err
	}
	console, err := executable("program", "console")
	if err != nil {
		return ready, err
	}
	session, err := signing.ActiveSecret(ctx, signkeys.ScopeDashboardSession)
	if err != nil {
		return ready, err
	}
	assertion, err := signing.ActiveSecret(ctx, signkeys.ScopeUserAssertion)
	if err != nil {
		return ready, err
	}
	if len(ready.ConsoleKeyFiles) != 1 {
		return ready, fmt.Errorf("offline console needs exactly one active token key")
	}
	token, err := os.ReadFile(ready.ConsoleKeyFiles[0])
	if err != nil {
		return ready, err
	}
	// The native console uses its restored signing keys and schema while admission
	// keeps login mutations and webhooks paused throughout inspection.
	consoleCA := workspace + "/platform-ca.crt"
	if err := writePrivate(consoleCA, oldCA); err != nil {
		return ready, err
	}
	env = map[string]string{"DASHBOARD_PROFILE": "production", "DASHBOARD_AUTHORITY_FILE": authorityFile, "DASHBOARD_DATABASE_URL": url, "DASHBOARD_DATABASE_SCHEMA": input.Config.ConsoleSchema, "DASHBOARD_JWT_SECRET": string(session), "DASHBOARD_CONTROLPLANE_USER_ASSERTION_SECRET": string(assertion), "DASHBOARD_GITHUB_TOKEN_ENCRYPTION_KEY": strings.TrimSpace(string(token)), "DASHBOARD_PUBLIC_BASE_URL": "https://console.localhost", "DASHBOARD_CONTROLPLANE_ADDRESS": "127.0.0.1:9443", "DASHBOARD_CONTROLPLANE_SERVER_NAME": "controlplane", "DASHBOARD_CONTROLPLANE_CA_FILE": consoleCA, "PORT": "3000", "HOST": "127.0.0.1"}
	if err := start("console", console, nil, env); err != nil {
		return ready, err
	}
	deadline = time.Now().Add(time.Minute)
	probes := []Probe{{URL: "http://127.0.0.1:9090/readyz", Status: http.StatusOK, BodyContains: `"ready"`}, {URL: "http://127.0.0.1:3000/readyz", Status: http.StatusOK, BodyContains: `"ready"`}, {URL: "https://127.0.0.1:5443/v2/", CAFile: certDir + "/ca.crt", Status: http.StatusOK}}
	for _, p := range probes {
		for {
			if _, err := probeHTTP(ctx, p, false); err == nil {
				break
			}
			if time.Now().After(deadline) {
				return ready, fmt.Errorf("offline platform did not become ready; inspect private process logs")
			}
			select {
			case <-ctx.Done():
				return ready, ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	ready.DatabaseURL, ready.Registry, ready.RegistryCertDir, ready.AvailabilityURL = url, "127.0.0.1:5443", certDir, "http://127.0.0.1:3000/readyz"
	ready.AuthFile = workspace + "/registry-auth.json"
	if err := writePrivate(ready.AuthFile, []byte(`{"auths":{}}`)); err != nil {
		return ready, err
	}
	return ready, nil
}
