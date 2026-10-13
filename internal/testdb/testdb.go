// Package testdb starts an ephemeral PostgreSQL server for tests and the
// local stack. It prefers, in order: PLATFORM_TEST_DATABASE_URL (an
// operator-provided server, never stopped), local PostgreSQL binaries
// (POSTGRES_BIN_DIR, PATH, or /usr/lib/postgresql/*/bin), then a Docker
// container running the pinned postgres image. Tests must not assume the
// server is empty beyond the databases they create themselves.
package testdb

import (
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// DefaultVersion is the pinned PostgreSQL major version for tests.
const DefaultVersion = "17"

const EnvVar = "DASHBOARD_TEST_DATABASE_URL"

// AdminEnvVar optionally points tests at an already-running server.
const AdminEnvVar = "PLATFORM_TEST_DATABASE_URL"

func defaultImage() string {
	if image := os.Getenv("POSTGRES_TEST_IMAGE"); image != "" {
		return image
	}
	return "postgres:" + DefaultVersion + "-alpine"
}

// Server is one ephemeral PostgreSQL server.
type Server struct {
	url  *url.URL
	stop func()
}

func (s *Server) PGURL() *url.URL {
	if s == nil || s.url == nil {
		return nil
	}
	clone := *s.url
	return &clone
}

func (s *Server) Stop() {
	if s != nil && s.stop != nil {
		s.stop()
	}
}

// Start starts a server. A non-empty version selects the Docker image tag
// when the Docker fallback is used; local binaries use whatever is installed.
func Start(version string) (*Server, error) {
	if raw := strings.TrimSpace(os.Getenv(AdminEnvVar)); raw != "" {
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", AdminEnvVar, err)
		}
		return &Server{url: u}, nil
	}
	if server, err := startLocal(); err == nil {
		return server, nil
	}
	return startDocker(version)
}

// StartEphemeral is Start for the local stack's short-lived database.
func StartEphemeral() (*Server, error) { return Start("") }

// Binaries returns the directory holding local initdb/postgres binaries,
// for tests that must drive a server process directly (custom TLS or
// configuration the ephemeral harness does not expose).
func Binaries() (string, bool) { return findBinaries() }

func findBinaries() (dir string, ok bool) {
	candidates := []string{os.Getenv("POSTGRES_BIN_DIR")}
	if initdb, err := exec.LookPath("initdb"); err == nil {
		candidates = append(candidates, filepath.Dir(initdb))
	}
	if matches, _ := filepath.Glob("/usr/lib/postgresql/*/bin"); len(matches) > 0 {
		sort.Strings(matches)
		candidates = append(candidates, matches[len(matches)-1])
	}
	if matches, _ := filepath.Glob("/opt/homebrew/opt/postgresql@*/bin"); len(matches) > 0 {
		sort.Strings(matches)
		candidates = append(candidates, matches[len(matches)-1])
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(candidate, "initdb")); err == nil {
			if _, err := os.Stat(filepath.Join(candidate, "postgres")); err == nil {
				return candidate, true
			}
		}
	}
	return "", false
}

func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func startLocal() (*Server, error) {
	binDir, ok := findBinaries()
	if !ok {
		return nil, fmt.Errorf("no local PostgreSQL binaries")
	}
	if os.Geteuid() == 0 {
		return nil, fmt.Errorf("PostgreSQL refuses to run as root")
	}
	root, err := os.MkdirTemp("", "platform-testdb-*")
	if err != nil {
		return nil, err
	}
	cleanup := func() { os.RemoveAll(root) }
	data := filepath.Join(root, "data")
	init := exec.Command(filepath.Join(binDir, "initdb"), "-D", data, "-U", "postgres", "--auth=trust", "--no-instructions")
	if out, err := init.CombinedOutput(); err != nil {
		cleanup()
		return nil, fmt.Errorf("initdb: %v: %s", err, out)
	}
	port, err := freePort()
	if err != nil {
		cleanup()
		return nil, err
	}
	logFile, err := os.Create(filepath.Join(root, "postgres.log"))
	if err != nil {
		cleanup()
		return nil, err
	}
	server := exec.Command(filepath.Join(binDir, "postgres"), "-D", data,
		"-c", fmt.Sprintf("port=%d", port), "-c", "listen_addresses=127.0.0.1",
		"-c", "unix_socket_directories="+root, "-c", "fsync=off", "-c", "wal_level=replica",
		"-c", "archive_mode=on", "-c", "archive_command=true", "-c", "max_wal_senders=3")
	server.Stdout, server.Stderr = logFile, logFile
	if err := server.Start(); err != nil {
		logFile.Close()
		cleanup()
		return nil, err
	}
	u := &url.URL{Scheme: "postgresql", User: url.User("postgres"), Host: fmt.Sprintf("127.0.0.1:%d", port), Path: "/postgres"}
	s := &Server{url: u, stop: func() {
		_ = server.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = server.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = server.Process.Kill()
			<-done
		}
		logFile.Close()
		cleanup()
	}}
	if err := waitReady(u.String()); err != nil {
		s.Stop()
		return nil, err
	}
	return s, nil
}

func startDocker(version string) (*Server, error) {
	docker, err := exec.LookPath("docker")
	if err != nil {
		return nil, fmt.Errorf("no local PostgreSQL binaries and docker is unavailable; set %s or POSTGRES_BIN_DIR", AdminEnvVar)
	}
	image := defaultImage()
	if version != "" && !strings.Contains(version, ":") {
		image = "postgres:" + strings.TrimPrefix(version, "v") + "-alpine"
	} else if strings.Contains(version, ":") {
		image = version
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	name := fmt.Sprintf("platform-testdb-%d-%d", os.Getpid(), port)
	run := exec.Command(docker, "run", "-d", "--rm", "--name", name,
		"-e", "POSTGRES_PASSWORD=testdb", "-e", "POSTGRES_USER=postgres",
		"-p", fmt.Sprintf("127.0.0.1:%d:5432", port), image,
		"-c", "fsync=off", "-c", "wal_level=replica", "-c", "archive_mode=on",
		"-c", "archive_command=true", "-c", "max_wal_senders=3")
	if out, err := run.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("docker run %s: %v: %s", image, err, out)
	}
	u := &url.URL{Scheme: "postgresql", User: url.UserPassword("postgres", "testdb"),
		Host: fmt.Sprintf("127.0.0.1:%d", port), Path: "/postgres"}
	s := &Server{url: u, stop: func() {
		_ = exec.Command(docker, "rm", "-f", name).Run()
	}}
	if err := waitReady(u.String()); err != nil {
		s.Stop()
		return nil, err
	}
	return s, nil
}

func waitReady(dsn string) error {
	deadline := time.Now().Add(60 * time.Second)
	for {
		db, err := sql.Open("pgx", dsn)
		if err == nil {
			err = db.Ping()
			db.Close()
			if err == nil {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("test PostgreSQL did not become ready: %w", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Open starts (or reuses the operator-provided) server and opens a database
// handle against database, creating it when missing. The returned stop
// function closes the handle and stops a server this call started.
func Open(database string) (*sql.DB, func(), error) {
	server, err := Start("")
	if err != nil {
		return nil, nil, err
	}
	adminURL := NormalizeURL(server.PGURL())
	admin, err := sql.Open("pgx", adminURL.String())
	if err != nil {
		server.Stop()
		return nil, nil, err
	}
	defer admin.Close()
	if database != "" && database != "postgres" {
		var exists bool
		if err := admin.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname=$1)`, database).Scan(&exists); err != nil {
			server.Stop()
			return nil, nil, err
		}
		if !exists {
			if _, err := admin.Exec(`CREATE DATABASE "` + strings.ReplaceAll(database, `"`, `""`) + `"`); err != nil {
				server.Stop()
				return nil, nil, err
			}
		}
	}
	target := *adminURL
	if database != "" {
		target.Path = "/" + database
	}
	db, err := sql.Open("pgx", target.String())
	if err != nil {
		server.Stop()
		return nil, nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		server.Stop()
		return nil, nil, err
	}
	return db, func() { db.Close(); server.Stop() }, nil
}

func NormalizeURL(source *url.URL) *url.URL {
	if source == nil {
		return nil
	}
	clone := *source
	query := clone.Query()
	if strings.TrimSpace(query.Get("sslmode")) == "" {
		query.Set("sslmode", "disable")
	}
	clone.RawQuery = query.Encode()
	return &clone
}
