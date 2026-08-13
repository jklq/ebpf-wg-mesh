package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ebof-wg-mesh/internal/health"
)

func TestControlPlaneReadinessOmitsPrivateTopology(t *testing.T) {
	t.Parallel()

	server := &Server{}
	rec := httptest.NewRecorder()
	health.ServeReadiness(server.readyReport)(rec, httptest.NewRequest(http.MethodGet, health.ReadinessPath, nil).WithContext(context.Background()))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", rec.Code)
	}
	var report health.Report
	if err := json.NewDecoder(rec.Body).Decode(&report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if report.Status != health.StatusNotReady {
		t.Fatalf("unexpected report %#v", report)
	}
	body := rec.Body.String()
	if strings.Contains(body, "postgresql://") || strings.Contains(body, "127.0.0.1") || strings.Contains(body, "password") {
		t.Fatalf("readiness leaked private topology: %s", body)
	}
	joined := strings.Join(report.Failed, ",")
	for _, name := range []string{"database", "migrations", "source_storage"} {
		if !strings.Contains(joined, name) {
			t.Fatalf("expected failed check %q in %q", name, joined)
		}
	}
}
