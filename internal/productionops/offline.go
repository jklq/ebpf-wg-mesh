package productionops

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/secretkeys"
	"ebof-wg-mesh/internal/controlplane/signkeys"
	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/platformdb"
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
	selected := map[string]string{}
	for _, requirement := range input.Point.Installer {
		selected[requirement.Kind] = requirement.ID
	}
	for _, d := range input.Point.Dependencies {
		if (d.Kind == "release" || d.Kind == "installation") && selected[d.Kind] != d.ID {
			continue
		}
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
	startAs := func(name, binary string, args []string, environment map[string]string, decorate func(*exec.Cmd)) error {
		log, err := os.OpenFile(filepath.Join(workspace, name+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer log.Close()
		cmd := exec.Command(binary, args...)
		cmd.Stdout, cmd.Stderr = log, log
		cmd.Dir = workspace
		cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "PLATFORM_RECOVERY_WORKSPACE=" + workspace}
		for key, value := range environment {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
		if decorate != nil {
			decorate(cmd)
		}
		if err := cmd.Start(); err != nil {
			return err
		}
		return saveJSON(filepath.Join(workspace, name+".pid"), cmd.Process.Pid)
	}
	start := func(name, binary string, args []string, environment map[string]string) error {
		return startAs(name, binary, args, environment, nil)
	}
	ca, err := certificate("offline-db-ca", nil, nil)
	if err != nil {
		return ready, err
	}
	node, err := certificate("node", &ca, []string{"127.0.0.1", "localhost", "database.offline.invalid"})
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
	postgres, err := executable("program", "postgresql")
	if err != nil {
		return ready, err
	}
	backrest, err := executable("tool", "pgbackrest")
	if err != nil {
		return ready, err
	}
	dataDir := filepath.Join(workspace, "data")
	if entries, err := os.ReadDir(dataDir); err == nil && len(entries) != 0 {
		return ready, fmt.Errorf("offline restore destination is not empty")
	} else if err != nil && !os.IsNotExist(err) {
		return ready, err
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return ready, err
	}
	if err := writePrivate(filepath.Join(workspace, "pg_hba.conf"), []byte(pgHBAConfig)); err != nil {
		return ready, err
	}
	repoRoot := filepath.Join(workspace, "database", strings.Trim(input.Config.BackupPrefix, "/"))
	restoreConfigPath := filepath.Join(workspace, "pgbackrest-restore.conf")
	if err := writePrivate(restoreConfigPath, []byte(recovery.RenderPgBackRestPosixConfig(input.Point.Database.Stanza, dataDir, repoRoot))); err != nil {
		return ready, err
	}
	// PostgreSQL and pgBackRest refuse to run as root; the drill namespace
	// may be root-owned, so database processes drop to nobody.
	drop, err := dropToNobody(dataDir, certDir, repoRoot, restoreConfigPath, filepath.Join(workspace, "pg_hba.conf"))
	if err != nil {
		return ready, err
	}
	restorer := platformdb.PgBackRest{Binary: backrest, ConfigFile: restoreConfigPath, Stanza: input.Point.Database.Stanza, Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = workspace
		cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "PLATFORM_RECOVERY_WORKSPACE=" + workspace}
		drop(cmd)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return out, fmt.Errorf("%s: %w: %s", name, err, out)
		}
		return out, nil
	}}
	if err := restorer.Restore(ctx, dataDir, input.Point.Database.BackupLabel, input.Point.Snapshot.Timestamp); err != nil {
		return ready, fmt.Errorf("offline pgBackRest restore: %w", err)
	}
	if err := startAs("database", postgres, []string{
		"-D", dataDir,
		"-c", "listen_addresses=127.0.0.1", "-c", "port=5432",
		"-c", "unix_socket_directories=" + workspace,
		"-c", "ssl=on",
		"-c", "ssl_cert_file=" + certDir + "/node.crt",
		"-c", "ssl_key_file=" + certDir + "/node.key",
		"-c", "ssl_ca_file=" + certDir + "/ca.crt",
		"-c", "hba_file=" + filepath.Join(workspace, "pg_hba.conf"),
		"-c", "shared_buffers=32MB", "-c", "max_connections=20",
	}, nil, drop); err != nil {
		return ready, err
	}
	url := fmt.Sprintf("postgresql://root@127.0.0.1:5432/postgres?sslmode=verify-full&sslrootcert=%s&sslcert=%s&sslkey=%s", certDir+"/ca.crt", certDir+"/client.root.crt", certDir+"/client.root.key")
	db, err := platformdb.OpenURL(ctx, url, platformdb.SchemaPlatform)
	if err != nil {
		return ready, err
	}
	defer db.Close()
	deadline := time.Now().Add(120 * time.Second)
	for {
		var inRecovery bool
		if err := db.QueryRowContext(ctx, `SELECT pg_is_in_recovery()`).Scan(&inRecovery); err == nil && !inRecovery {
			break
		}
		if time.Now().After(deadline) {
			return ready, fmt.Errorf("offline database startup/promotion did not converge")
		}
		select {
		case <-ctx.Done():
			return ready, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	var databaseName string
	if err := db.QueryRowContext(ctx, `SELECT datname FROM pg_database WHERE NOT datistemplate AND datname<>'postgres' ORDER BY datname LIMIT 1`).Scan(&databaseName); err != nil {
		return ready, fmt.Errorf("restored platform database is missing: %w", err)
	}
	url = strings.Replace(url, "/postgres?", "/"+databaseName+"?", 1)
	db.Close()
	db, err = platformdb.OpenURL(ctx, url, platformdb.SchemaPlatform)
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
				Version int                        `json:"version"`
				Keys    map[string]json.RawMessage `json:"keys"`
			}
			if err := privateJSON(input.Files["keyring/"+d.ID], &part); err != nil {
				return ready, err
			}
			if part.Version != 1 {
				return ready, fmt.Errorf("unsupported protected keyring version")
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
	durable := "/var/lib/platform-offline"
	env := map[string]string{"CONTROLPLANE_PROFILE": "production", "CONTROLPLANE_DB_URL": strings.Replace(url, "127.0.0.1:5432", "database.offline.invalid:5432", 1), "CONTROLPLANE_SECRET_KEYS_KEYRING": durable + "/keyring.json", "CONTROLPLANE_AUTHORITY_FILE": authorityFile, "CONTROLPLANE_STATE_DIR": durable + "/controlplane", "CONTROLPLANE_HEALTH_LISTEN": "127.0.0.1:9090", "CONTROLPLANE_INTERNAL_LISTEN": "127.0.0.1:9443", "CONTROLPLANE_INTERNAL_SERVER_NAMES": "controlplane.offline.invalid,127.0.0.1", "CONTROLPLANE_CONSOLE_CALLER_IDS": "offline-console", "CONTROLPLANE_DASHBOARD_ENABLED": "false", "CONTROLPLANE_SOURCE_ARCHIVES_DIR": durable + "/sources", "CONTROLPLANE_INGRESS_PLATFORM_TLS_CERT_FILE": certDir + "/node.crt", "CONTROLPLANE_INGRESS_PLATFORM_TLS_KEY_FILE": certDir + "/node.key", "CONTROLPLANE_INGRESS_PUBLIC_ADDR": "platform.offline.invalid"}
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
	client, err := identity.IssueClientCertificate(ctx, signing, identity.CallerDashboard, "offline-console", time.Hour)
	if err != nil {
		return ready, err
	}
	for name, data := range map[string][]byte{"console-client.crt": client.CertPEM, "console-client.key": client.KeyPEM} {
		if err = writePrivate(filepath.Join(workspace, name), data); err != nil {
			return ready, err
		}
	}
	env = map[string]string{"DASHBOARD_PROFILE": "production", "DASHBOARD_AUTHORITY_FILE": authorityFile, "DASHBOARD_DATABASE_URL": strings.Replace(url, "127.0.0.1:5432", "database.offline.invalid:5432", 1), "DASHBOARD_DATABASE_SCHEMA": input.Config.ConsoleSchema, "DASHBOARD_JWT_SECRET": string(session), "DASHBOARD_CONTROLPLANE_USER_ASSERTION_SECRET": string(assertion), "DASHBOARD_GITHUB_TOKEN_ENCRYPTION_KEY": strings.TrimSpace(string(token)), "DASHBOARD_PUBLIC_BASE_URL": "https://console.offline.invalid", "DASHBOARD_INGRESS_TARGET_HOST": "console.offline.invalid", "DASHBOARD_CONTROLPLANE_ADDRESSES": "controlplane.offline.invalid:9443", "DASHBOARD_CONTROLPLANE_SERVER_NAME": "controlplane.offline.invalid", "DASHBOARD_CONTROLPLANE_CERT_FILE": workspace + "/console-client.crt", "DASHBOARD_CONTROLPLANE_KEY_FILE": workspace + "/console-client.key", "DASHBOARD_CONTROLPLANE_CA_FILE": consoleCA, "PORT": "3000", "HOST": "127.0.0.1"}
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

// dropToNobody returns a command decorator that runs database processes as
// nobody when the drill itself runs as root (PostgreSQL refuses root), and
// chowns the paths those processes must read or write. When unprivileged
// already it is a no-op.
func dropToNobody(paths ...string) (func(*exec.Cmd), error) {
	noop := func(*exec.Cmd) {}
	if os.Geteuid() != 0 {
		return noop, nil
	}
	nobody, err := user.Lookup("nobody")
	if err != nil {
		return noop, fmt.Errorf("offline database requires a nobody account to drop root: %w", err)
	}
	uid, err1 := strconv.Atoi(nobody.Uid)
	gid, err2 := strconv.Atoi(nobody.Gid)
	if err1 != nil || err2 != nil {
		return noop, fmt.Errorf("invalid nobody account ids")
	}
	for _, path := range paths {
		err := filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			return os.Chown(p, uid, gid)
		})
		if err != nil && !os.IsNotExist(err) {
			return noop, err
		}
	}
	return func(cmd *exec.Cmd) {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
	}, nil
}
