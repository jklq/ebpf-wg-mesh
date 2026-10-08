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
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"ebof-wg-mesh/internal/deploy"
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
		for name, b := range map[string][]byte{"ca.crt": ca.Certificate, "node.crt": node.Certificate, "node.key": node.Key} {
			path := cfgDir(r.Plan, pl) + "/certs/" + name
			if verify {
				script += verifyRemoteFile(path, b)
			} else {
				script += remoteFile(path, b)
			}
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

func (r *Runner) initializeDatabase(ctx context.Context, restore bool) error {
	if r.Plan.Recovery != restore {
		return fmt.Errorf("fresh initialization and restore destination initialization are distinct")
	}
	if !restore && r.Plan.Previous != nil {
		return fmt.Errorf("database-init is only valid for a fresh installation")
	}
	if err := r.verifyDatabaseInitialized(ctx); err != nil {
		initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(initCtx, r.Config.Database.Binary, "init", "--host="+r.reachableDatabase(ctx), "--certs-dir="+r.databasePKI())
		if output, err := cmd.CombinedOutput(); err != nil {
			path := filepath.Join(r.Config.StateDirectory, r.Plan.ID, "native-init.log")
			if len(output) > 64<<10 {
				output = output[:64<<10]
			}
			_ = writePrivate(path, output)
			return fmt.Errorf("native cluster initialization failed: %w (private diagnostics: %s)", err, path)
		}
	}
	if restore {
		return r.verifyEmptyDatabase(ctx)
	}
	db, err := r.db(ctx, true)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err = db.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS "+r.Config.Database.Name); err != nil {
		return err
	}
	if _, err = db.ExecContext(ctx, `CREATE DATABASE IF NOT EXISTS platform_recovery`); err != nil {
		return err
	}
	if _, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS platform_recovery.public.bootstrap_admission(installation STRING PRIMARY KEY,generation STRING NOT NULL,database_name STRING NOT NULL)`); err != nil {
		return err
	}
	var generation, name string
	err = db.QueryRowContext(ctx, `SELECT generation,database_name FROM platform_recovery.public.bootstrap_admission WHERE installation=$1`, r.Plan.Installation.ID).Scan(&generation, &name)
	if err == sql.ErrNoRows {
		var count int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM [SHOW TABLES FROM "+r.Config.Database.Name+"]").Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("fresh installation will not admit an occupied platform database")
		}
		_, err = db.ExecContext(ctx, `INSERT INTO platform_recovery.public.bootstrap_admission VALUES($1,$2,$3)`, r.Plan.Installation.ID, r.Plan.Generation, r.Config.Database.Name)
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
func (r *Runner) verifyEmptyDatabase(ctx context.Context) error {
	db, err := r.db(ctx, true)
	if err != nil {
		return err
	}
	defer db.Close()
	return recovery.CheckEmptyDestination(ctx, db)
}

func (r *Runner) databaseStatus(ctx context.Context) (deploy.DatabaseStatus, error) {
	var status deploy.DatabaseStatus
	file := filepath.Join(r.Config.StateDirectory, "native-plan.json")
	b, _ := json.Marshal(r.Plan)
	if err := writePrivate(file, b); err != nil {
		return status, err
	}
	var output strings.Builder
	args := []string{"--plan", file, "--binary", r.Config.Database.Binary, "--host", r.reachableDatabase(ctx), "--certs-dir", r.databasePKI(), "--databases", "system," + r.Config.Database.Name}
	if !r.Plan.Automatic {
		args = append(args, "--require-convergence")
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
	if err := db.QueryRowContext(ctx, `SELECT generation,database_name FROM platform_recovery.public.bootstrap_admission WHERE installation=$1`, r.Plan.Installation.ID).Scan(&generation, &name); err != nil {
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
			hosts = append(hosts, net.JoinHostPort(h.Network.Address, "26257"))
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
	return addresses[0] // The native TLS command supplies the authoritative error.
}
