package agent

import (
	"strings"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
)

func TestRenderServiceHostsFileProvidesFullAndShortInternalNames(t *testing.T) {
	t.Parallel()

	contents, err := renderServiceHostsFile([]*agentv1.InternalHost{
		{Hostname: "worker.mesh.internal", Ipv6: "fd00:20::20"},
		{Hostname: "accurate-reflection.mesh.internal", Ipv6: "fd00:20::10"},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(contents)
	for _, line := range []string{
		"fd00:20::10 accurate-reflection.mesh.internal accurate-reflection",
		"fd00:20::20 worker.mesh.internal worker",
	} {
		if !strings.Contains(text, line+"\n") {
			t.Fatalf("hosts file %q does not contain %q", text, line)
		}
	}
}

func TestRenderServiceHostsFilePublishesMultipleAddressesForOneService(t *testing.T) {
	t.Parallel()

	contents, err := renderServiceHostsFile([]*agentv1.InternalHost{
		{Hostname: "web.mesh.internal", Ipv6: "fd00:20::11"},
		{Hostname: "web.mesh.internal", Ipv6: "fd00:20::10"},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(contents)
	for _, line := range []string{
		"fd00:20::10 web.mesh.internal web",
		"fd00:20::11 web.mesh.internal web",
	} {
		if !strings.Contains(text, line+"\n") {
			t.Fatalf("hosts file %q does not contain %q", text, line)
		}
	}
}

func TestRenderServiceHostsFileRejectsNonMeshHosts(t *testing.T) {
	t.Parallel()

	_, err := renderServiceHostsFile([]*agentv1.InternalHost{{
		Hostname: "public.example.com",
		Ipv6:     "fd00:20::10",
	}})
	if err == nil {
		t.Fatal("expected a non-mesh hostname to be rejected")
	}
}
