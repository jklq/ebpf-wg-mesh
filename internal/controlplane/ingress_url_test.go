package controlplane

import (
	"ebof-wg-mesh/internal/controlplane/routing"
	"testing"
)

func TestIngressConfigAPIURL(t *testing.T) {
	t.Parallel()

	got := routing.ConfigAPIURL("http://127.0.0.1:2019/load", "/config/apps/http/servers/srv0/routes")
	want := "http://127.0.0.1:2019/config/apps/http/servers/srv0/routes"
	if got != want {
		t.Fatalf("routing.ConfigAPIURL = %q, want %q", got, want)
	}
	if got := routing.ConfigAPIURL("not a url", "/config/"); got != "" {
		t.Fatalf("expected empty url for invalid admin url, got %q", got)
	}
}
