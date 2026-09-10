package journal

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestAuthorizeSchedulingRequiresFenceForAuthority(t *testing.T) {
	store := &Store{}
	if _, err := store.authorizeScheduling(context.Background(), nil, assignedBatch()); err == nil {
		t.Fatal("authority-bearing batch was authorized without a fence")
	}
}

func TestAuthorizeSchedulingSkipsStagedOnlyBatches(t *testing.T) {
	store := &Store{}
	staged := Batch{Deployments: []Change[Deployment]{{Key: "deployment", Value: &Deployment{ID: "deployment", State: "staged"}}}}
	epoch, err := store.authorizeScheduling(context.Background(), nil, staged)
	if err != nil {
		t.Fatalf("staged batch required authority: %v", err)
	}
	if epoch != nil {
		t.Fatalf("staged batch produced epoch %d", *epoch)
	}
}

func TestAuthorizeSchedulingReturnsFenceEpoch(t *testing.T) {
	store := &Store{fence: func(context.Context, *sql.Tx) (int64, error) { return 7, nil }}
	epoch, err := store.authorizeScheduling(context.Background(), nil, assignedBatch())
	if err != nil {
		t.Fatal(err)
	}
	if epoch == nil || *epoch != 7 {
		t.Fatalf("epoch = %v, want 7", epoch)
	}
}

func TestAuthorizeSchedulingRejectsNonPositiveEpoch(t *testing.T) {
	store := &Store{fence: func(context.Context, *sql.Tx) (int64, error) { return 0, nil }}
	if _, err := store.authorizeScheduling(context.Background(), nil, assignedBatch()); err == nil {
		t.Fatal("epoch 0 was accepted as scheduling authority")
	}
}

func TestAuthorizeSchedulingPropagatesFenceError(t *testing.T) {
	sentinel := errors.New("authority unavailable")
	store := &Store{fence: func(context.Context, *sql.Tx) (int64, error) { return 0, sentinel }}
	if _, err := store.authorizeScheduling(context.Background(), nil, assignedBatch()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}
