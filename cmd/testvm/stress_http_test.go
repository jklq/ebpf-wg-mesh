package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"
)

func TestRemoteHTTPGeneratorChecksContent(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("expected-marker\n")) }))
	defer server.Close()
	for _, marker := range []string{"expected-marker", "wrong-marker"} {
		out, err := exec.Command(python, "../../infra/test-vm/remote/stress-http.py", "5", "2", "1", server.URL, marker, "2").CombinedOutput()
		if err != nil {
			t.Fatalf("generator: %v: %s", err, out)
		}
		var report stressHTTPReport
		if err := json.Unmarshal(out, &report); err != nil {
			t.Fatal(err)
		}
		if report.Requests != 4 || report.RPS <= 0 {
			t.Fatalf("generator requests = %d, want 4", report.Requests)
		}
		if len(report.Buckets) != 60001 {
			t.Fatalf("generator did not report a histogram: %d buckets", len(report.Buckets))
		}
		if marker == "expected-marker" && report.Errors != 0 {
			t.Fatalf("valid content failed: %s", out)
		}
		if marker == "wrong-marker" && (report.Errors != report.Requests || report.Codes["wrong-content"] != report.Requests) {
			t.Fatalf("wrong content accepted: %s", out)
		}
	}
}
