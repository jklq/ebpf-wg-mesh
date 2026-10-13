package productionops

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/platformdb"
	"ebof-wg-mesh/internal/recovery"
)

type certificateBundle struct{ Certificate, Key []byte }

func certificate(name string, ca *certificateBundle, hosts []string) (certificateBundle, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return certificateBundle{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return certificateBundle{}, err
	}
	t := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(365 * 24 * time.Hour), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			t.IPAddresses = append(t.IPAddresses, ip)
		} else {
			t.DNSNames = append(t.DNSNames, h)
		}
	}
	parent := t
	signer := key
	if ca == nil {
		t.IsCA = true
		t.KeyUsage |= x509.KeyUsageCertSign | x509.KeyUsageCRLSign
		t.NotAfter = time.Now().Add(10 * 365 * 24 * time.Hour)
	} else {
		b, _ := pem.Decode(ca.Certificate)
		if b == nil {
			return certificateBundle{}, fmt.Errorf("invalid CA certificate")
		}
		parent, err = x509.ParseCertificate(b.Bytes)
		if err != nil {
			return certificateBundle{}, err
		}
		k, _ := pem.Decode(ca.Key)
		if k == nil {
			return certificateBundle{}, fmt.Errorf("invalid CA key")
		}
		v, err := x509.ParsePKCS8PrivateKey(k.Bytes)
		if err != nil {
			return certificateBundle{}, err
		}
		var ok bool
		signer, ok = v.(*ecdsa.PrivateKey)
		if !ok {
			return certificateBundle{}, fmt.Errorf("invalid CA key type")
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, t, parent, key.Public(), signer)
	if err != nil {
		return certificateBundle{}, err
	}
	k, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return certificateBundle{}, err
	}
	return certificateBundle{pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: k})}, nil
}
func loadCertificate(path, name string, ca *certificateBundle, hosts []string, verify bool) (certificateBundle, error) {
	var b certificateBundle
	if err := privateJSON(path, &b); err == nil {
		pair, err := tls.X509KeyPair(b.Certificate, b.Key)
		if err != nil {
			return b, fmt.Errorf("existing certificate/key mismatch")
		}
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return b, err
		}
		if leaf.Subject.CommonName != name {
			return b, fmt.Errorf("certificate identity mismatch")
		}
		if ca != nil {
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(ca.Certificate)
			if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
				return b, err
			}
			for _, host := range hosts {
				if err := leaf.VerifyHostname(host); err != nil {
					return b, err
				}
			}
		}
		if time.Until(leaf.NotAfter) > 48*time.Hour {
			return b, nil
		}
		if verify {
			return b, fmt.Errorf("certificate renewal required")
		}
	} else if !os.IsNotExist(err) {
		return b, err
	} else if verify {
		return b, err
	}
	b, err := certificate(name, ca, hosts)
	if err != nil {
		return b, err
	}
	raw, _ := json.Marshal(b)
	return b, writePrivate(path, raw)
}
func (r *Runner) databasePKI() string {
	return filepath.Join(r.Config.Database.CertificateDirectory, r.Plan.Generation)
}
func remoteFile(path string, b []byte) string {
	return "mkdir -p " + shell(filepath.Dir(path)) + "\nprintf %s " + shell(base64.StdEncoding.EncodeToString(b)) + " | base64 -d > " + shell(path+".next") + "\nchmod 0600 " + shell(path+".next") + "\nmv -f " + shell(path+".next") + " " + shell(path) + "\n"
}
func verifyRemoteFile(path string, b []byte) string {
	digest := sha256.Sum256(b)
	return "if ! printf %s " + shell(hex.EncodeToString(digest[:])+"  "+path+"\n") + " | sha256sum -c - >/dev/null; then printf '%s\\n' " + shell("private input verification failed: "+path) + " >&2; exit 1; fi\n"
}

func (r *Runner) databaseCredentials(ctx context.Context, verify bool) error {
	ca, err := loadCertificate(filepath.Join(r.databasePKI(), "ca.json"), "database-ca", nil, nil, verify)
	if err != nil {
		return err
	}
	root, err := loadCertificate(filepath.Join(r.databasePKI(), "root.json"), "root", &ca, nil, verify)
	if err != nil {
		return err
	}
	for name, b := range map[string][]byte{"ca.crt": ca.Certificate, "client.root.crt": root.Certificate, "client.root.key": root.Key} {
		path := filepath.Join(r.databasePKI(), name)
		if verify {
			raw, err := os.ReadFile(path)
			if err != nil || string(raw) != string(b) {
				return fmt.Errorf("database admin credential differs from CA")
			}
		} else if err := writePrivate(path, b); err != nil {
			return err
		}
	}
	for _, pl := range r.Plan.Placements {
		if pl.Role != deploy.Database {
			continue
		}
		if r.Plan.Automatic {
			continue
		}
		h, _ := r.Plan.Installation.Host(pl.Host)
		node, err := loadCertificate(filepath.Join(r.databasePKI(), pl.Instance+".json"), "node", &ca, []string{h.Network.Address, "localhost", "127.0.0.1"}, verify)
		if err != nil {
			return err
		}
		var script string
		backrest, err := r.pgBackRestConfig(dataDir(r.Plan, pl) + "/data")
		if err != nil {
			return err
		}
		files := map[string][]byte{"ca.crt": ca.Certificate, "node.crt": node.Certificate, "node.key": node.Key, "pg_hba.conf": []byte(pgHBAConfig), "pgbackrest.conf": []byte(backrest)}
		for _, name := range sortedFiles(files) {
			b := files[name]
			path := cfgDir(r.Plan, pl) + "/certs/" + name
			if verify {
				script += verifyRemoteFile(path, b)
			} else {
				script += remoteFile(path, b)
			}
		}
		activation := credentialRuntimeEnvironment(deploy.Database, files, nil)
		if verify {
			script += verifyRemoteFile(cfgDir(r.Plan, pl)+"/runtime.env", activation)
		} else {
			script += remoteFile(cfgDir(r.Plan, pl)+"/runtime.env", activation)
		}
		probe, ok := r.Config.Probes[deploy.Database]
		if !ok {
			return fmt.Errorf("database startup probe required")
		}
		probe = r.expandProbe(probe, pl)
		b, _ := json.Marshal(probe)
		if verify {
			script += verifyRemoteFile(cfgDir(r.Plan, pl)+"/probe.json", b)
		} else {
			script += remoteFile(cfgDir(r.Plan, pl)+"/probe.json", b)
		}
		script += r.databaseHostScript(pl, verify)
		if _, err := r.remote(ctx, r.Plan, pl, script); err != nil {
			return err
		}
	}
	u, err := verifiedDatabaseURL("root", r.databaseAddresses(), r.Config.Database.Name, filepath.Join(r.databasePKI(), "ca.crt"), filepath.Join(r.databasePKI(), "client.root.crt"), filepath.Join(r.databasePKI(), "client.root.key"))
	if err != nil {
		return err
	}
	if !verify {
		return writePrivate(r.Config.Database.URLFile, []byte(u.String()))
	}
	b, err := os.ReadFile(r.Config.Database.URLFile)
	if err != nil || string(b) != u.String() {
		return fmt.Errorf("database admin URL differs from provisioned identity")
	}
	return nil
}

// pgHBAConfig is the whole authentication policy of the primary: local peer
// for the service account (pgBackRest and operator maintenance on the host)
// and TLS client-certificate authentication for everyone else. Certificate
// CN is the SQL user, matching the credentials operations issues.
const pgHBAConfig = `local   all   root   peer
hostssl all   all    0.0.0.0/0   cert clientcert=verify-full
hostssl all   all    ::0/0       cert clientcert=verify-full
`

// databaseTool is a release tool's path on a fleet host.
func (r *Runner) databaseTool(name string) string {
	return "/opt/ebpf-wg-mesh/" + r.Plan.Installation.ID + "/" + r.Plan.Release.ID + "/tools/" + name
}

// pgBackRestConfig renders the shared stanza configuration for pgPath (the
// data directory on the database host, or empty for repository-only use on
// a completer).
func (r *Runner) pgBackRestConfig(pgPath string) (string, error) {
	c, s, err := recoveryConfig(r.Config.RecoveryConfig)
	if err != nil {
		return "", err
	}
	clear(s.RecoveryKey)
	return recovery.RenderPgBackRestConfig(c.Storage, c.BackupPrefix, r.Config.Database.PgBackRestStanza, pgPath)
}

// databaseHostScript prepares (or verifies) the OS account, ownership, and
// data directory PostgreSQL needs before its unit can start: the server
// refuses root, and initdb must happen exactly once, before first start.
// A restored installation also passes through here; restore replaces the
// initialized contents only after the empty-destination check.
func (r *Runner) databaseHostScript(pl deploy.Placement, verify bool) string {
	data := dataDir(r.Plan, pl) + "/data"
	certs := cfgDir(r.Plan, pl) + "/certs"
	owner := deploy.DatabaseOSUser
	if verify {
		return "test -f " + shell(data+"/PG_VERSION") + "\n" +
			"test \"$(stat -c %U " + shell(data) + ")\" = " + shell(owner) + "\n" +
			"test \"$(stat -c %U " + shell(certs+"/node.key") + ")\" = " + shell(owner) + "\n"
	}
	script := "id -u " + shell(owner) + " >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin " + shell(owner) + "\n"
	script += "mkdir -p " + shell(data) + " " + shell(dataDir(r.Plan, pl)+"/pgbackrest-spool") + " " + shell(dataDir(r.Plan, pl)+"/pgbackrest-lock") + " " + shell(dataDir(r.Plan, pl)+"/pgbackrest-log") + "\n"
	script += "chown " + shell(owner) + " " + shell(dataDir(r.Plan, pl)) + " " + shell(data) + " " + shell(dataDir(r.Plan, pl)+"/pgbackrest-spool") + " " + shell(dataDir(r.Plan, pl)+"/pgbackrest-lock") + " " + shell(dataDir(r.Plan, pl)+"/pgbackrest-log") + "\n"
	script += "chown -R " + shell(owner) + " " + shell(certs) + "\n"
	script += "chmod 0700 " + shell(data) + "\n"
	script += "if ! test -f " + shell(data+"/PG_VERSION") + "; then runuser -u " + shell(owner) + " -- " + shell(r.Config.Database.InitDBBinary) + " -D " + shell(data) + " -U root --data-checksums --auth=trust --no-instructions >/dev/null; fi\n"
	return script
}

func (r *Runner) initializeDatabase(ctx context.Context, restore bool) error {
	if r.Plan.Recovery != restore {
		return fmt.Errorf("fresh initialization and restore destination initialization are distinct")
	}
	if !restore && r.Plan.Previous != nil {
		return fmt.Errorf("database-init is only valid for a fresh installation")
	}
	if err := r.verifyDatabaseInitialized(ctx); err != nil {
		return fmt.Errorf("database primary is not accepting SQL; its data directory is prepared by database-credentials before first start: %w", err)
	}
	if restore {
		return r.verifyEmptyDatabase(ctx)
	}
	admin, err := r.db(ctx, true)
	if err != nil {
		return err
	}
	var exists bool
	if err := admin.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname=$1)`, r.Config.Database.Name).Scan(&exists); err != nil {
		admin.Close()
		return err
	}
	if !exists {
		if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+platformdb.QuoteIdentifier(r.Config.Database.Name)); err != nil {
			admin.Close()
			return err
		}
	}
	admin.Close()
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	provider, err := platformdb.For(r.Config.Database.Provider)
	if err != nil {
		return err
	}
	for _, schema := range platformdb.Schemas {
		if err := provider.EnsureSchema(ctx, db, schema); err != nil {
			return err
		}
	}
	if _, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS recovery.bootstrap_admission(installation TEXT PRIMARY KEY,generation TEXT NOT NULL,database_name TEXT NOT NULL)`); err != nil {
		return err
	}
	// The node certificate identity exists only so the local readiness probe
	// can run SELECT 1; it owns nothing and reads nothing.
	if _, err = db.ExecContext(ctx, `DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='node') THEN CREATE ROLE node LOGIN; END IF; END $$`); err != nil {
		return err
	}
	var generation, name string
	err = db.QueryRowContext(ctx, `SELECT generation,database_name FROM recovery.bootstrap_admission WHERE installation=$1`, r.Plan.Installation.ID).Scan(&generation, &name)
	if err == sql.ErrNoRows {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_type='BASE TABLE' AND table_schema='platform'`).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("fresh installation will not admit an occupied platform database")
		}
		_, err = db.ExecContext(ctx, `INSERT INTO recovery.bootstrap_admission VALUES($1,$2,$3)`, r.Plan.Installation.ID, r.Plan.Generation, r.Config.Database.Name)
		return err
	}
	if err != nil {
		return err
	}
	if generation != r.Plan.Generation || name != r.Config.Database.Name {
		return fmt.Errorf("fresh database admission differs from installation generation")
	}
	return nil
}
func (r *Runner) verifyDatabaseInitialized(ctx context.Context) error {
	db, err := r.db(ctx, true)
	if err != nil {
		return err
	}
	defer db.Close()
	var ready int
	return db.QueryRowContext(ctx, "SELECT 1").Scan(&ready)
}

// verifyEmptyDatabase proves the restore destination cluster holds nothing
// but the freshly initialized empty cluster: no user databases beyond the
// platform database, and no tables in its platform schemas.
func (r *Runner) verifyEmptyDatabase(ctx context.Context) error {
	admin, err := r.db(ctx, true)
	if err != nil {
		return err
	}
	defer admin.Close()
	var unexpected int
	if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_database WHERE NOT datistemplate AND datname NOT IN ('postgres',$1)`, r.Config.Database.Name).Scan(&unexpected); err != nil {
		return err
	}
	if unexpected != 0 {
		return fmt.Errorf("restore destination contains user-created databases")
	}
	var exists bool
	if err := admin.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname=$1)`, r.Config.Database.Name).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	return recovery.CheckEmptyDestination(ctx, db)
}

func (r *Runner) databaseStatus(ctx context.Context) (deploy.DatabaseStatus, error) {
	return r.inspectDatabase(ctx, r.Plan, !r.Plan.Automatic)
}

func (r *Runner) inspectDatabase(ctx context.Context, plan deploy.Plan, live bool) (deploy.DatabaseStatus, error) {
	var status deploy.DatabaseStatus
	file := filepath.Join(r.Config.StateDirectory, "native-plan.json")
	b, _ := json.Marshal(plan)
	if err := writePrivate(file, b); err != nil {
		return status, err
	}
	var output strings.Builder
	args := []string{"--plan", file, "--url-file", r.Config.Database.URLFile}
	if live {
		args = append(args, "--require-live")
	}
	if err := deploy.RunDatabaseStatus(ctx, args, &output); err != nil {
		return status, err
	}
	err := json.Unmarshal([]byte(output.String()), &status)
	return status, err
}

func (r *Runner) verifyFreshDatabaseReady(ctx context.Context) error {
	if r.Plan.Recovery {
		return fmt.Errorf("fresh readiness cannot satisfy a restore")
	}
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	var generation, name string
	if err := db.QueryRowContext(ctx, `SELECT generation,database_name FROM recovery.bootstrap_admission WHERE installation=$1`, r.Plan.Installation.ID).Scan(&generation, &name); err != nil {
		return err
	}
	if generation != r.Plan.Generation || name != r.Config.Database.Name {
		return fmt.Errorf("fresh database was not admitted for this plan")
	}
	return nil
}

// A service may select an external SQL endpoint; otherwise native libpq
// multi-host failover covers the actual database placements.
func (r *Runner) databaseAddresses() string {
	if r.Config.Database.Address != "" {
		return r.Config.Database.Address
	}
	var hosts []string
	for _, pl := range r.Plan.Placements {
		if pl.Role == deploy.Database {
			h, _ := r.Plan.Installation.Host(pl.Host)
			hosts = append(hosts, net.JoinHostPort(h.Network.Address, "5432"))
		}
	}
	return strings.Join(hosts, ",")
}
func (r *Runner) reachableDatabase(ctx context.Context) string {
	addresses := strings.Split(r.databaseAddresses(), ",")
	for _, address := range addresses {
		c, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", address)
		if err == nil {
			c.Close()
			return address
		}
	}
	return addresses[0] // The SQL probe supplies the authoritative error.
}
