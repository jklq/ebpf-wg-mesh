package health

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"time"
)

const (
	LivenessPath  = "/livez"
	ReadinessPath = "/readyz"
)

const (
	StatusLive     = "live"
	StatusReady    = "ready"
	StatusNotReady = "not_ready"
)

type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type Report struct {
	Status string   `json:"status"`
	Failed []string `json:"failed,omitempty"`
}

type Probe func(context.Context) Report

func NewMux(ready Probe) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc(LivenessPath, ServeLiveness)
	mux.HandleFunc(ReadinessPath, ServeReadiness(ready))
	return mux
}

func ServeLiveness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Report{Status: StatusLive})
}

func ServeReadiness(ready Probe) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ready == nil {
			writeJSON(w, http.StatusOK, Report{Status: StatusReady})
			return
		}
		report := ready(r.Context())
		if report.Status == "" {
			if len(report.Failed) == 0 {
				report.Status = StatusReady
			} else {
				report.Status = StatusNotReady
			}
		}
		status := http.StatusOK
		if report.Status != StatusReady {
			status = http.StatusServiceUnavailable
			report.Status = StatusNotReady
		}
		writeJSON(w, status, report)
	}
}

func ListenAndServe(ctx context.Context, listen string, ready Probe) (addr string, shutdown func(context.Context) error, err error) {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return "", nil, err
	}
	server := &http.Server{
		Handler:           NewMux(ready),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       time.Minute,
	}
	go func() {
		_ = server.Serve(ln)
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	return ln.Addr().String(), server.Shutdown, nil
}

func writeJSON(w http.ResponseWriter, status int, report Report) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(report)
}
