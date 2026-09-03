package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

func buildBinaries(ctx context.Context, repoRoot string, binaries map[string]string) error {
	builds := map[string]string{
		"./cmd/controlplane":         binaries["controlplane"],
		"./cmd/agent":                binaries["agent"],
		"./cmd/internal-client-cert": binaries["internal-client-cert"],
	}
	for pkg, output := range builds {
		if err := runCommand(ctx, repoRoot, append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0"), "go", "build", "-o", output, pkg); err != nil {
			return err
		}
	}
	return nil
}

func readHostsOutput(ctx context.Context, repoRoot string, env []string, tofuBin, tofuDir string) (map[string]hostInfo, error) {
	cmd := exec.CommandContext(ctx, tofuBin, "-chdir="+tofuDir, "output", "-json", "hosts")
	cmd.Dir = repoRoot
	cmd.Env = env
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var hosts map[string]hostInfo
	if err := json.Unmarshal(output, &hosts); err != nil {
		return nil, err
	}
	return hosts, nil
}

func selectCheapestServerType(ctx context.Context, client *hcloud.Client, locationName string) (string, error) {
	location, _, err := client.Location.GetByName(ctx, locationName)
	if err != nil {
		return "", err
	}
	if location == nil {
		return "", fmt.Errorf("unknown location %q", locationName)
	}

	serverTypes, err := client.ServerType.All(ctx)
	if err != nil {
		return "", err
	}

	bestName := ""
	bestPrice := 0.0
	for _, serverType := range serverTypes {
		if serverType == nil {
			continue
		}
		if serverType.Architecture != hcloud.ArchitectureX86 {
			continue
		}
		if serverType.CPUType != hcloud.CPUTypeShared {
			continue
		}
		if !serverTypeAvailableAtLocation(serverType, location.Name) {
			continue
		}
		price, ok := serverTypeHourlyGross(serverType, location.Name)
		if !ok {
			continue
		}
		if bestName == "" || price < bestPrice || (price == bestPrice && serverType.Name < bestName) {
			bestName = serverType.Name
			bestPrice = price
		}
	}

	if bestName == "" {
		return "", fmt.Errorf("no orderable shared x86 server types found for location %q", location.Name)
	}
	return bestName, nil
}

func serverTypeAvailableAtLocation(serverType *hcloud.ServerType, locationName string) bool {
	for _, loc := range serverType.Locations {
		if loc.Location == nil || loc.Location.Name != locationName {
			continue
		}
		return !loc.IsDeprecated()
	}
	return false
}

func serverTypeHourlyGross(serverType *hcloud.ServerType, locationName string) (float64, bool) {
	for _, pricing := range serverType.Pricings {
		if pricing.Location == nil || pricing.Location.Name != locationName {
			continue
		}
		value, err := strconv.ParseFloat(pricing.Hourly.Gross, 64)
		if err != nil {
			return 0, false
		}
		return value, true
	}
	return 0, false
}

func collectArtifacts(ctx context.Context, repoRoot, artifactRoot, keyPath string, hosts map[string]hostInfo) error {
	for name, host := range hosts {
		hostDir := filepath.Join(artifactRoot, "hosts", name)
		if err := os.MkdirAll(hostDir, 0o755); err != nil {
			return err
		}
		commands := map[string]string{
			"journal.txt":   "journalctl -u ebpf-wg-mesh-controlplane -u ebpf-wg-mesh-controlplane-replica -u ebpf-wg-mesh-ingress-probe -u ebpf-wg-mesh-agent -u ebpf-wg-mesh-cockroach --no-pager || true",
			"ctr.txt":       "ctr --namespace default containers list || true; ctr --namespace default tasks list || true",
			"wg.txt":        "wg show || true",
			"network.txt":   "ip -brief addr || true; ss -ltnup || true",
			"systemd.txt":   "systemctl status ebpf-wg-mesh-controlplane ebpf-wg-mesh-controlplane-replica ebpf-wg-mesh-ingress-probe ebpf-wg-mesh-agent ebpf-wg-mesh-cockroach --no-pager || true",
			"processes.txt": "ps aux | grep -E 'controlplane|agent|cockroach|containerd' | grep -v grep || true",
		}
		if host.Role == "controlplane" {
			commands["db.txt"] = "cockroach sql --insecure --host=127.0.0.1:26257 --execute \"SELECT * FROM projects; SELECT * FROM project_memberships; SELECT * FROM agents; SELECT * FROM control_plane_leases; SELECT * FROM control_plane_storage;\" || true"
			commands["ingress-probe.txt"] = "printf '%s\\n' '=== requests ==='; cat /var/lib/ebpf-wg-mesh/ingress-probe/requests.log 2>/dev/null || true; printf '%s\\n' '=== latest ==='; cat /var/lib/ebpf-wg-mesh/ingress-probe/latest.json 2>/dev/null || true"
		}
		for fileName, remoteCmd := range commands {
			out, err := runRemoteCommand(ctx, keyPath, host.PublicIPv4, remoteCmd)
			if err != nil {
				out = append(out, []byte("\nERROR: "+err.Error()+"\n")...)
			}
			if writeErr := os.WriteFile(filepath.Join(hostDir, fileName), out, 0o644); writeErr != nil {
				return writeErr
			}
		}
	}
	return nil
}

func identityMaterial(identity clientIdentity) ([]byte, []byte, []byte, error) {
	ca, err := decodeB64(identity.CAPEMB64)
	if err != nil {
		return nil, nil, nil, err
	}
	cert, err := decodeB64(identity.CertPEMB64)
	if err != nil {
		return nil, nil, nil, err
	}
	key, err := decodeB64(identity.KeyPEMB64)
	if err != nil {
		return nil, nil, nil, err
	}
	return ca, cert, key, nil
}

func decodeB64(value string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(value)
}

func trimCIDR(value string) string {
	return strings.TrimSpace(strings.SplitN(value, "/", 2)[0])
}

func writeJSON(path string, value any) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func runCommand(ctx context.Context, dir string, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func failf(format string, args ...any) {
	panic(fmt.Sprintf(format, args...))
}

func infof(format string, args ...any) {
	fmt.Printf("[testvm] "+format+"\n", args...)
}
