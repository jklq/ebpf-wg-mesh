package bootstrap

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"ebof-wg-mesh/internal/controlplane"
)

func RunInstallationDatabase(args []string) error {
	if len(args) == 0 || !strings.Contains("|bootstrap|convert|", "|"+args[0]+"|") {
		return fmt.Errorf("usage: controlplane database <bootstrap|convert> [flags]")
	}
	fs := flag.NewFlagSet("controlplane database "+args[0], flag.ContinueOnError)
	var dbURL, keyring, backup, cutoff, revocations, consoleSchema string
	var from int
	stringFlag(fs, &dbURL, "db-url", "CONTROLPLANE_DATABASE_URL", "", "secure database URL")
	stringFlag(fs, &keyring, "keyring", "CONTROLPLANE_SECRET_KEYS_KEYRING", "", "externally retained master keyring path")
	fs.IntVar(&from, "from-version", 0, "source schema version for a flat conversion")
	fs.StringVar(&backup, "backup", "", "verified complete backup location")
	fs.StringVar(&cutoff, "data-loss-cutoff", "", "backup mutation cutoff, RFC3339")
	fs.StringVar(&revocations, "revocations-file", "", "v41 revoked client serials to import into shared state")
	fs.StringVar(&consoleSchema, "console-schema", "dashboard", "console SQL schema included in the flat conversion")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || dbURL == "" {
		return fmt.Errorf("database operation requires --db-url and no positional arguments")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		return err
	}
	defer db.Close()
	if args[0] == "bootstrap" {
		if keyring == "" {
			return fmt.Errorf("explicit bootstrap requires --keyring")
		}
		return controlplane.BootstrapInstallation(ctx, db, keyring)
	}
	t, err := time.Parse(time.RFC3339, cutoff)
	if err != nil {
		return fmt.Errorf("conversion requires --data-loss-cutoff as RFC3339")
	}
	var serials []string
	if revocations != "" {
		b, err := os.ReadFile(revocations)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
			if line != "" {
				serials = append(serials, line)
			}
		}
	}
	return controlplane.ConvertInstallationSchema(ctx, db, controlplane.Conversion{FromSchema: from, Backup: backup, DataLossCutoff: t, ConsoleSchema: consoleSchema, RevokedSerials: serials})
}
