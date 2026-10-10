package productionops

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"

	"ebof-wg-mesh/internal/deploy"
)

// Input is the NUL-separated running process environment. Never print it: SQL
// URLs and the rest of the environment contain component credentials.
func inspectSQLFailover(ctx context.Context, role deploy.Role, excluded string, input io.Reader) error {
	data, err := io.ReadAll(io.LimitReader(input, 2<<20))
	if err != nil {
		return err
	}
	env := map[string]string{}
	for _, item := range strings.Split(string(data), "\x00") {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			env[key] = value
		}
	}
	var candidates []string
	switch role {
	case deploy.ControlPlane:
		u, err := url.Parse(env["CONTROLPLANE_DB_URL"])
		if err != nil || u.Scheme != "postgresql" {
			return fmt.Errorf("running control plane has no verified SQL configuration")
		}
		hosts, ports := strings.Split(u.Query().Get("host"), ","), strings.Split(u.Query().Get("port"), ",")
		if u.Query().Get("host") == "" {
			hosts, ports = []string{u.Hostname()}, []string{u.Port()}
		}
		if len(hosts) != len(ports) {
			return fmt.Errorf("SQL host and port lists differ")
		}
		for n, host := range hosts {
			peer := *u
			selectDatabaseHost(&peer, net.JoinHostPort(host, ports[n]))
			candidates = append(candidates, peer.String())
		}
	case deploy.Console:
		if err := json.Unmarshal([]byte(env["DASHBOARD_DATABASE_URLS"]), &candidates); err != nil {
			return fmt.Errorf("running console has no SQL failover endpoints")
		}
	default:
		return fmt.Errorf("SQL failover inspection requires a SQL client role")
	}
	for _, candidate := range candidates {
		u, err := url.Parse(candidate)
		if err != nil || u.Scheme != "postgresql" || u.Query().Get("sslmode") != "verify-full" {
			return fmt.Errorf("SQL failover requires verified TLS")
		}
		if u.Host == excluded {
			continue
		}
		if err := probeSQL(ctx, u.String()); err == nil {
			return nil
		}
	}
	return fmt.Errorf("running client has no reachable SQL endpoint outside the restarting node")
}

func inspectSQLClient(ctx context.Context, path, schema string) error {
	if !regexp.MustCompile(`^[a-z][a-z0-9_]*$`).MatchString(schema) {
		return fmt.Errorf("invalid console schema")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	u, err := url.Parse(strings.TrimSpace(string(b)))
	if err != nil || u.Scheme != "postgresql" || u.Query().Get("sslmode") != "verify-full" {
		return fmt.Errorf("component SQL client requires verified TLS")
	}
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		return err
	}
	defer db.Close()
	if err = db.PingContext(ctx); err != nil {
		return fmt.Errorf("component SQL TLS authentication failed")
	}
	for _, query := range []string{"SELECT count(*) FROM public.schema_migrations", "SELECT count(*) FROM " + schema + ".schema_migrations", "SELECT count(*) FROM public.recovery_runtime_authority"} {
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
