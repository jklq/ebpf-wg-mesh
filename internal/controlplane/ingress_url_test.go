package controlplane

import "testing"

func TestIngressConfigAPIURL(t *testing.T) {
	t.Parallel()

	got := ingressConfigAPIURL("http://127.0.0.1:2019/load", "/config/apps/http/servers/srv0/routes")
	want := "http://127.0.0.1:2019/config/apps/http/servers/srv0/routes"
	if got != want {
		t.Fatalf("ingressConfigAPIURL = %q, want %q", got, want)
	}
	if got := ingressConfigAPIURL("not a url", "/config/"); got != "" {
		t.Fatalf("expected empty url for invalid admin url, got %q", got)
	}
}
