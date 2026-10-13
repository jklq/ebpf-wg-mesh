package productionops

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"

	"ebof-wg-mesh/internal/platformdb"
)

// inspectSQLClient verifies one component's certificate client against its
// own schema, plus the platform pause authority both components must read.
func inspectSQLClient(ctx context.Context, path, schema string) error {
	if !regexp.MustCompile(`^[a-z][a-z0-9_]*$`).MatchString(schema) {
		return fmt.Errorf("invalid component schema")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	u, err := url.Parse(strings.TrimSpace(string(b)))
	if err != nil || u.Scheme != "postgresql" || u.Query().Get("sslmode") != "verify-full" {
		return fmt.Errorf("component SQL client requires verified TLS")
	}
	db, err := platformdb.OpenURL(ctx, u.String(), platformdb.SchemaPlatform)
	if err != nil {
		return fmt.Errorf("component SQL TLS authentication failed: %w", err)
	}
	defer db.Close()
	for _, query := range []string{"SELECT count(*) FROM " + schema + ".schema_migrations", "SELECT count(*) FROM platform.recovery_runtime_authority"} {
		var count int
		if err = db.QueryRowContext(ctx, query).Scan(&count); err != nil {
			return fmt.Errorf("component SQL permissions are incomplete")
		}
		if count != 1 {
			return fmt.Errorf("component SQL schema/authority differs")
		}
	}
	return nil
}

// IPv6 comma lists are not valid URL authorities for net/url. Keep a valid
// primary authority and use pgx's explicit host/port query lists for failover.
// Each fallback constructs its own verify-full TLS server name.
func verifiedDatabaseURL(user, addresses, database, ca, cert, key string) (*url.URL, error) {
	peers := strings.Split(addresses, ",")
	var hosts, ports []string
	for _, address := range peers {
		host, port, err := net.SplitHostPort(address)
		if err != nil || host == "" || port == "" {
			return nil, fmt.Errorf("invalid database peer address")
		}
		hosts = append(hosts, host)
		ports = append(ports, port)
	}
	u := &url.URL{Scheme: "postgresql", User: url.User(user), Host: peers[0], Path: "/" + database}
	q := url.Values{"sslmode": {"verify-full"}, "sslrootcert": {ca}, "sslcert": {cert}, "sslkey": {key}, "connect_timeout": {"3"}}
	if len(peers) > 1 {
		q.Set("host", strings.Join(hosts, ","))
		q.Set("port", strings.Join(ports, ","))
	}
	u.RawQuery = q.Encode()
	return u, nil
}

// Bun/node-postgres and native single-host admin commands use a selected peer
// before submitting a request. Never leave a failover list overriding that peer.
func selectDatabaseHost(u *url.URL, address string) {
	u.Host = address
	q := u.Query()
	q.Del("host")
	q.Del("port")
	u.RawQuery = q.Encode()
}
