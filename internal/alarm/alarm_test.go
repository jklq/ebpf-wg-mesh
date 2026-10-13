package alarm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSendPostsEventJSON(t *testing.T) {
	var got Event
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			t.Errorf("unexpected request: %s %s", r.Method, r.Header.Get("Content-Type"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	sender, err := New(server.URL, "controlplane")
	if err != nil {
		t.Fatal(err)
	}
	err = sender.Send(context.Background(), Event{
		Event: "certificate_renewal_failed", Subject: "app.example.net",
		Message: "issuer said  no", NextAction: "check DNS",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Event != "certificate_renewal_failed" || got.Source != "controlplane" || got.Subject != "app.example.net" || got.Message != "issuer said no" || got.NextAction != "check DNS" || got.At.IsZero() {
		t.Fatalf("event = %+v", got)
	}
}

func TestSendReportsWebhookFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	sender, err := New(server.URL, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.Send(context.Background(), Event{Event: "x"}); err == nil {
		t.Fatal("expected an error for a 500 webhook answer")
	}
}

func TestDisabledSenderSendsNothing(t *testing.T) {
	sender, err := New("", "test")
	if err != nil {
		t.Fatal(err)
	}
	if sender.Enabled() {
		t.Fatal("empty URL must disable alarms")
	}
	if err := sender.Send(context.Background(), Event{Event: "x"}); err != nil {
		t.Fatal(err)
	}
	var nilSender *Sender
	if err := nilSender.Send(context.Background(), Event{Event: "x"}); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsUnsafeURLs(t *testing.T) {
	for _, raw := range []string{
		"http://hooks.example.com/alarm",
		"https://user:pass@hooks.example.com/alarm",
		"ftp://hooks.example.com/alarm",
		"https://",
	} {
		if err := Validate(raw); err == nil {
			t.Errorf("Validate(%q) succeeded, want error", raw)
		}
	}
	for _, raw := range []string{"", "https://hooks.example.com/alarm?token=1", "http://127.0.0.1:9099/alarm"} {
		if err := Validate(raw); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", raw, err)
		}
	}
}
