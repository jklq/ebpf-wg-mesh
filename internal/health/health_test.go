package health

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLivenessDoesNotExposeTopology(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	ServeLiveness(rec, httptest.NewRequest(http.MethodGet, LivenessPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var report Report
	if err := json.NewDecoder(rec.Body).Decode(&report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if report.Status != StatusLive {
		t.Fatalf("unexpected report %#v", report)
	}
	if strings.Contains(rec.Body.String(), "127.0.0.1") || strings.Contains(rec.Body.String(), "password") {
		t.Fatalf("liveness leaked private data: %s", rec.Body.String())
	}
}

func TestReadinessReportsFailedDependencyClassesOnly(t *testing.T) {
	t.Parallel()

	handler := ServeReadiness(func(context.Context) Report {
		return Report{Failed: []string{"database", "migrations"}}
	})
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, ReadinessPath, nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"failed":["database","migrations"]`) {
		t.Fatalf("unexpected body %s", body)
	}
	if strings.Contains(body, "postgresql://") || strings.Contains(body, "cockroach") {
		t.Fatalf("readiness leaked private topology: %s", body)
	}
}
