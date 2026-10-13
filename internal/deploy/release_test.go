package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"ebof-wg-mesh/internal/alarm"

	"go.yaml.in/yaml/v3"
)

// failReleaseDriver fails the final production verification of one release,
// leaving an interrupted upgrade that the release command must handle alone.
type failReleaseDriver struct {
	*fakeDriver
	releaseID string
}

func (d *failReleaseDriver) Execute(ctx context.Context, p Plan, state State, op Operation) (Binding, error) {
	if p.Release.ID == d.releaseID && op.Hook == "production-verify" {
		return Binding{}, errors.New("injected production verification failure")
	}
	return d.fakeDriver.Execute(ctx, p, state, op)
}

type releaseAlarmSink struct {
	mu     sync.Mutex
	events []alarm.Event
}

func (s *releaseAlarmSink) got() []alarm.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]alarm.Event(nil), s.events...)
}

// runReleaseCommand installs r42, then runs `platformctl release` onto the
// next release through the real CLI and returns its output, the committed
// state, and the alarms it sent.
func runReleaseCommand(t *testing.T, mutate func(*Release), driver Driver) (string, State, []alarm.Event, error) {
	t.Helper()
	i, r, inv := fixture(3)
	state := applied(build(t, i, r, State{}, inv, false))
	next := nextRelease(r, "r44")
	if mutate != nil {
		mutate(&next)
	}
	i.Release = "r44"

	dir := t.TempDir()
	manifest, bundle, keyFile := filepath.Join(dir, "production.yaml"), filepath.Join(dir, "r44.yaml"), filepath.Join(dir, "key")
	for path, value := range map[string]any{manifest: i, bundle: next} {
		data, err := yaml.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store, key, statePath := testStore(t)
	if err := store.Write(state); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, key, 0o600); err != nil {
		t.Fatal(err)
	}

	sink := &releaseAlarmSink{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var event alarm.Event
		if err := json.NewDecoder(req.Body).Decode(&event); err != nil {
			t.Errorf("decode alarm: %v", err)
		}
		sink.mu.Lock()
		sink.events = append(sink.events, event)
		sink.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	var out bytes.Buffer
	err := Run(context.Background(), []string{
		"release", "--installation", manifest, "--bundle", bundle,
		"--state", statePath, "--key-file", keyFile, "--alarm-webhook-url", server.URL,
	}, &out, driver)

	after, openErr := OpenState(statePath, key)
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer after.Close()
	final, readErr := after.Read()
	if readErr != nil {
		t.Fatal(readErr)
	}
	return out.String(), final, sink.got(), err
}

func TestReleaseCommandAppliesNewReleaseInOneAction(t *testing.T) {
	_, _, inv := fixture(3)
	out, final, events, err := runReleaseCommand(t, nil, fake(inv))
	if err != nil {
		t.Fatal(err)
	}
	if final.Progress != nil || final.Bundle.ID != "r44" {
		t.Fatalf("release did not commit r44: bundle %v progress %v", final.Bundle, final.Progress)
	}
	if !strings.Contains(out, "release r44 applied") {
		t.Fatalf("output = %q", out)
	}
	if len(events) != 0 {
		t.Fatalf("events = %+v, want no alarm on success", events)
	}
}

func TestReleaseCommandRollsBackAloneWhenVerificationFails(t *testing.T) {
	_, _, inv := fixture(3)
	driver := &failReleaseDriver{fakeDriver: fake(inv), releaseID: "r44"}
	out, final, events, err := runReleaseCommand(t, nil, driver)
	if err == nil || !strings.Contains(err.Error(), "rolled back to r42") {
		t.Fatalf("err = %v, want a rolled-back release failure", err)
	}
	if final.Progress != nil || final.Bundle.ID != "r42" {
		t.Fatalf("rollback did not commit r42: bundle %v progress %v", final.Bundle, final.Progress)
	}
	if !strings.Contains(out, "rolled back to r42") {
		t.Fatalf("output = %q", out)
	}
	if len(events) != 1 || events[0].Event != "release_failed_rolled_back" || !strings.Contains(events[0].Subject, "r42 -> r44") {
		t.Fatalf("events = %+v, want one rolled-back alarm", events)
	}
}

func TestReleaseCommandRefusesRollbackAcrossExpandedSchemas(t *testing.T) {
	_, _, inv := fixture(3)
	driver := &failReleaseDriver{fakeDriver: fake(inv), releaseID: "r44"}
	_, final, events, err := runReleaseCommand(t, func(next *Release) {
		next.Schema++
		next.SchemaChanges = map[string]SchemaChange{"r42": {
			Expand:   Hook{Command: []string{"/opt/expand"}, Verify: []string{"/opt/check-expand"}},
			Backfill: Hook{Command: []string{"/opt/backfill"}, Verify: []string{"/opt/check-backfill"}},
		}}
	}, driver)
	if err == nil || !strings.Contains(err.Error(), "without a safe automatic rollback") {
		t.Fatalf("err = %v, want the forward-repair refusal", err)
	}
	if final.Progress == nil || final.Bundle.ID != "r42" {
		t.Fatalf("interrupted release must stay resumable on r42: bundle %v progress %v", final.Bundle, final.Progress)
	}
	if len(events) != 1 || events[0].Event != "release_failed" || !strings.Contains(events[0].NextAction, "repair") {
		t.Fatalf("events = %+v, want one repair-forward alarm", events)
	}
}
