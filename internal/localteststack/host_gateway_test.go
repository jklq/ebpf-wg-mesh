package localteststack

import (
	"testing"
)

func TestHostGatewayIPLabelParsesBuildxInspect(t *testing.T) {
	t.Parallel()

	sample := []byte(`
Name:          desktop-linux
Driver:        docker
Nodes:
Name:             desktop-linux
 org.mobyproject.buildkit.worker.moby.host-gateway-ip: 192.168.65.254
 org.mobyproject.buildkit.worker.network:              host
`)
	match := hostGatewayIPLabel.FindSubmatch(sample)
	if len(match) != 2 {
		t.Fatalf("expected host-gateway match, got %#v", match)
	}
	if got := string(match[1]); got != "192.168.65.254" {
		t.Fatalf("unexpected gateway IP %q", got)
	}
}
