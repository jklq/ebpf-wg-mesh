package productionops

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A component host receives only its probe's selected TLS material. Operator
// paths need not exist there, including after independent installer recovery.
// Generated component identities already reside in the component directory.
func localizeProbe(probe Probe, directory string) (Probe, map[string][]byte, error) {
	files := map[string][]byte{}
	for _, material := range []struct {
		path *string
		name string
	}{{&probe.CAFile, "probe-ca.crt"}, {&probe.CertFile, "probe-client.crt"}, {&probe.KeyFile, "probe-client.key"}} {
		path := *material.path
		if path == "" || strings.HasPrefix(path, directory+"/") {
			continue
		}
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.Contains(path, "{") {
			return Probe{}, nil, fmt.Errorf("probe TLS material must name a resolved private file")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return Probe{}, nil, err
		}
		files[material.name] = data
		*material.path = directory + "/" + material.name
	}
	return probe, files, nil
}
