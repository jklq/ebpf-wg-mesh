package productionops

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"ebof-wg-mesh/internal/alarm"
	"ebof-wg-mesh/internal/deploy"
)

func TestCredentialRenewalFailureAlarmsTheOperator(t *testing.T) {
	var mu sync.Mutex
	var events []alarm.Event
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event alarm.Event
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Errorf("decode alarm: %v", err)
		}
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	runner := Runner{
		Plan:   deploy.Plan{Installation: deploy.Installation{ID: "production"}},
		Config: Config{AlarmWebhookURL: server.URL},
	}
	boom := errors.New("database unavailable")
	if err := runner.alarmFailure(context.Background(), "credential-renew", boom); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the original failure", err)
	}
	if err := runner.alarmFailure(context.Background(), "backup", boom); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the original failure", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 || events[0].Event != "credential_renewal_failed" || events[0].Subject != "production" {
		t.Fatalf("events = %+v, want one credential renewal alarm", events)
	}
}

func TestAlarmWebhookURLIsValidatedInOperationsConfig(t *testing.T) {
	b, err := os.ReadFile("../../infra/production/operations/config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.AlarmWebhookURL == "" {
		t.Fatal("the reference configuration must show the alarm destination")
	}
	cfg.AlarmWebhookURL = "http://hooks.example.com/alarm"
	if err := cfg.Validate(); err == nil {
		t.Fatal("plain HTTP to a public alarm webhook must be rejected")
	}
}
