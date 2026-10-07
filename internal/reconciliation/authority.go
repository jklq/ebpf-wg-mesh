package reconciliation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Authority is provisioned through host administration, never learned through
// RPC or endpoint discovery. ClusterID identifies the current CA trust root;
// InstallationID remains stable when recovery replaces that root.
type Authority struct {
	InstallationID string `json:"installationId"`
	Generation     string `json:"generation"`
	ClusterID      string `json:"clusterId"`
	Paused         bool   `json:"paused"`
	Checkpoints    bool   `json:"checkpoints"`
}

func ReadAuthority(path string) (Authority, error) {
	var a Authority
	if !filepath.IsAbs(path) {
		return a, fmt.Errorf("authority file must be an absolute host-admin path")
	}
	info, err := os.Stat(path)
	if err != nil {
		return a, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return a, fmt.Errorf("authority file must be a private regular file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return a, err
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return a, err
	}
	if a.InstallationID == "" || a.Generation == "" || a.ClusterID == "" {
		return a, fmt.Errorf("authority requires installation identity, recovery generation and CA identity")
	}
	return a, nil
}
