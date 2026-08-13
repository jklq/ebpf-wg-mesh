package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/restartpolicy"

	"google.golang.org/protobuf/encoding/protojson"
)

func (r *ContainerdRuntime) observationDir() string {
	return filepath.Join(r.cfg.Runtime.DataDir, "restarts")
}

func (r *ContainerdRuntime) loadObservation(allocationID string, remote *platformv1.RestartObservation) *platformv1.RestartObservation {
	local, err := readObservationFile(r.observationDir(), allocationID)
	if err != nil {
		return restartpolicy.MergeObservations(nil, remote)
	}
	return restartpolicy.MergeObservations(local, remote)
}

func (r *ContainerdRuntime) saveObservation(allocationID string, obs *platformv1.RestartObservation) error {
	if err := os.MkdirAll(r.observationDir(), 0o755); err != nil {
		return fmt.Errorf("mkdir restart dir: %w", err)
	}
	return writeObservationFile(r.observationDir(), allocationID, obs)
}

func (r *ContainerdRuntime) removeObservation(allocationID string) error {
	return removeObservationFile(r.observationDir(), allocationID)
}

func readObservationFile(dir, allocationID string) (*platformv1.RestartObservation, error) {
	path, err := runtimeChildPath(dir, "allocation ID", allocationID)
	if err != nil {
		return nil, err
	}
	path += ".json"
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	obs := &platformv1.RestartObservation{}
	if err := protojson.Unmarshal(raw, obs); err != nil {
		return nil, err
	}
	return obs, nil
}

func writeObservationFile(dir, allocationID string, obs *platformv1.RestartObservation) error {
	path, err := runtimeChildPath(dir, "allocation ID", allocationID)
	if err != nil {
		return err
	}
	path += ".json"
	if obs == nil {
		return os.Remove(path)
	}
	raw, err := protojson.Marshal(obs)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		buf.Reset()
		buf.Write(raw)
	}
	buf.WriteByte('\n')
	current, err := os.ReadFile(path)
	if err == nil && bytes.Equal(current, buf.Bytes()) {
		return os.Chmod(path, 0o600)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read restart observation %s: %w", allocationID, err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func removeObservationFile(dir, allocationID string) error {
	path, err := runtimeChildPath(dir, "allocation ID", allocationID)
	if err != nil {
		return err
	}
	if err := os.Remove(path + ".json"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
