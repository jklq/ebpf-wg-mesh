//go:build integration

package sqlretry

import (
	"context"
	"database/sql"
	"fmt"
	_ "github.com/jackc/pgx/v5/stdlib"
	"os"
	"sync"
	"testing"
	"time"
)

func TestPostgresSerializableRetry(t *testing.T) {
	raw := os.Getenv("CONTROLPLANE_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("set CONTROLPLANE_TEST_DATABASE_URL for PostgreSQL transaction checks")
	}
	db, err := sql.Open("pgx", raw)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	table := fmt.Sprintf("retry_bench_%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, "CREATE TABLE "+table+" (n INT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	defer db.Exec("DROP TABLE " + table)
	if _, err := db.ExecContext(ctx, "INSERT INTO "+table+" VALUES (0)"); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	failures := make(chan error, 4)
	for i := 0; i < 4; i++ {
		workers.Go(func() {
			for j := 0; j < 20; j++ {
				if err := ExecuteTx(ctx, db, nil, func(tx *sql.Tx) error {
					var n int
					if err := tx.QueryRowContext(ctx, "SELECT n FROM "+table).Scan(&n); err != nil {
						return err
					}
					_, err := tx.ExecContext(ctx, "UPDATE "+table+" SET n=$1", n+1)
					return err
				}); err != nil {
					failures <- err
					return
				}
			}
		})
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, "SELECT n FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 80 {
		t.Fatalf("lost updates: %d", n)
	}
}
