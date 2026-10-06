package journal

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestSQLRowAndReplayUseCanonicalTimes(t *testing.T) {
	value, err := decodeValue[Project]([]byte(`{"id":"p","created_at":"2026-10-07T00:00:00+00:00"}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var replay Project
	if err := json.Unmarshal(raw, &replay); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(value, replay) || value.CreatedAt.Location() != time.UTC {
		t.Fatalf("SQL row differs from replay: %+v %+v", value, replay)
	}
}
