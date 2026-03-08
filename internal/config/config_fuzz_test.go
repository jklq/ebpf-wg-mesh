package config

import (
	"strings"
	"testing"
)

func FuzzSplitCSV_Normalization(f *testing.F) {
	f.Add("")
	f.Add("a,b,c")
	f.Add(" , a, b ,, c ")
	f.Add("one")

	f.Fuzz(func(t *testing.T, raw string) {
		items := splitCSV(raw)
		for _, item := range items {
			if item == "" {
				t.Fatalf("splitCSV returned empty element for %q", raw)
			}
			if strings.TrimSpace(item) != item {
				t.Fatalf("splitCSV returned untrimmed element %q for %q", item, raw)
			}
		}
	})
}

func FuzzValidate_NoPanic(f *testing.F) {
	f.Add("node-a", "private-key", 51820, "10.0.0.1/24")
	f.Add("", "", 0, "bad-cidr")

	f.Fuzz(func(t *testing.T, nodeName, privateKey string, listenPort int, address string) {
		cfg := Config{
			NodeName: nodeName,
			WireGuard: WireGuard{
				PrivateKey: privateKey,
				ListenPort: listenPort,
				Addresses:  []string{address},
			},
			Containerd: ContainerdConfig{
				ProjectLabel: "mesh.project_id",
				IPv6Label:    "mesh.ipv6",
			},
		}
		applyDefaults(&cfg)
		_ = validate(cfg)
	})
}
