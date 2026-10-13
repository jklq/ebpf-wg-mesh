package certificates

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"ebof-wg-mesh/internal/alarm"
)

type alarmSink struct {
	mu     sync.Mutex
	events []alarm.Event
	server *httptest.Server
}

func newAlarmSink(t *testing.T) *alarmSink {
	t.Helper()
	sink := &alarmSink{}
	sink.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event alarm.Event
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Errorf("decode alarm: %v", err)
		}
		sink.mu.Lock()
		sink.events = append(sink.events, event)
		sink.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(sink.server.Close)
	return sink
}

func (s *alarmSink) sender(t *testing.T) *alarm.Sender {
	t.Helper()
	sender, err := alarm.New(s.server.URL, "test")
	if err != nil {
		t.Fatal(err)
	}
	return sender
}

func (s *alarmSink) got() []alarm.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]alarm.Event(nil), s.events...)
}

func TestRenewalFailureAlarmsWhenNoCertificateIsServed(t *testing.T) {
	t.Parallel()

	sink := newAlarmSink(t)
	h := newHarness(t, []Hostname{{Name: "app.example.com"}}, func(cfg *Config) {
		cfg.Alarm = sink.sender(t)
	})
	h.issuer.err = errors.New("CA unreachable")
	h.reconcile(t)

	events := sink.got()
	if len(events) != 1 || events[0].Event != "certificate_renewal_failed" || events[0].Subject != "app.example.com" {
		t.Fatalf("events = %+v, want one renewal failure for the hostname", events)
	}
}

func TestRenewalFailureStaysSilentWithAmpleLifetimeLeft(t *testing.T) {
	t.Parallel()

	sink := newAlarmSink(t)
	h := newHarness(t, []Hostname{{Name: "app.example.com"}}, func(cfg *Config) {
		cfg.Alarm = sink.sender(t)
	})
	h.reconcile(t)
	record := h.store.records["app.example.com"]
	if record.Fingerprint == "" {
		t.Fatal("first issuance must succeed")
	}
	// Renewal comes due at two thirds of the 90-day lifetime, with about
	// 30 days still served: a failure now retries silently.
	*h.now = record.RenewAt
	h.issuer.err = errors.New("CA unreachable")
	h.reconcile(t)
	if h.store.records["app.example.com"].Attempts != 1 {
		t.Fatalf("record = %+v, want the failed renewal attempt recorded", h.store.records["app.example.com"])
	}
	if events := sink.got(); len(events) != 0 {
		t.Fatalf("events = %+v, want no alarm while ample lifetime remains", events)
	}

	// Inside the alarm window the same failure pages the operator.
	*h.now = record.NotAfter.Add(-24 * time.Hour)
	h.reconcile(t)
	events := sink.got()
	if len(events) != 1 || events[0].Event != "certificate_renewal_failed" {
		t.Fatalf("events = %+v, want one alarm near expiry", events)
	}
}

func TestPlatformWildcardExpiryAlarmsOncePerLevel(t *testing.T) {
	t.Parallel()

	sink := newAlarmSink(t)
	ca := newTestCA(t)
	wildcard := ca.issue(t, time.Now().Add(-time.Hour), 6*24*time.Hour, "*.apps.example.net")
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, wildcard.ChainPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, wildcard.KeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := New(Config{
		PlatformSuffix: "apps.example.net", PlatformCertFile: certFile, PlatformKeyFile: keyFile,
		Alarm: sink.sender(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	service.checkPlatform(context.Background())
	service.checkPlatform(context.Background())
	events := sink.got()
	if len(events) != 1 || events[0].Event != "platform_certificate_expiring" || events[0].Subject != "*.apps.example.net" {
		t.Fatalf("events = %+v, want exactly one expiring alarm for the wildcard", events)
	}
}
