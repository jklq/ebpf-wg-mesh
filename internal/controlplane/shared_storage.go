package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

const controlPlaneStorageMarker = ".control-plane-storage-id"

// verifySharedControlPlaneDirectory binds a filesystem directory to the
// CockroachDB cluster. Replicas using the same shared mount observe the same
// marker. A replica accidentally using node-local storage gets a different
// marker and fails startup before it can issue incompatible certificates or
// advertise source objects that other replicas cannot read.
func verifySharedControlPlaneDirectory(ctx context.Context, store *persistence, name, directory string) error {
	if store == nil || store.db == nil {
		return errors.New("control-plane store is required")
	}
	directory = filepath.Clean(strings.TrimSpace(directory))
	if directory == "" || directory == "." {
		return fmt.Errorf("control-plane %s directory is required", name)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create control-plane %s directory: %w", name, err)
	}
	markerPath := filepath.Join(directory, controlPlaneStorageMarker)
	storageID, err := readOrCreateStorageMarker(markerPath)
	if err != nil {
		return fmt.Errorf("initialize control-plane %s storage marker: %w", name, err)
	}

	var registered string
	err = store.withTxUnfenced(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO control_plane_storage(name, storage_id, created_at)
			VALUES ($1, $2, statement_timestamp()) ON CONFLICT(name) DO NOTHING`, name, storageID); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT storage_id FROM control_plane_storage WHERE name = $1`, name).Scan(&registered)
	})
	if err != nil {
		return fmt.Errorf("register control-plane %s storage: %w", name, err)
	}
	if registered != storageID {
		return fmt.Errorf("control-plane %s directory is not the shared directory registered by this database", name)
	}
	return nil
}

func readOrCreateStorageMarker(path string) (string, error) {
	if raw, err := os.ReadFile(path); err == nil {
		return validateStorageID(raw)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	storageID := uuid.NewString()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return "", readErr
		}
		return validateStorageID(raw)
	}
	if err != nil {
		return "", err
	}
	if _, err := file.WriteString(storageID + "\n"); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return storageID, nil
}

func validateStorageID(raw []byte) (string, error) {
	storageID := strings.TrimSpace(string(raw))
	if _, err := uuid.Parse(storageID); err != nil {
		return "", fmt.Errorf("invalid storage marker: %w", err)
	}
	return storageID, nil
}
