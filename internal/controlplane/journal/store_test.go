package journal

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
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

func TestPublicationRejectsOlderReadsAndTrimsAlreadyAppliedBatches(t *testing.T) {
	s := New(nil, "test", nil)
	var updates []Applied
	s.publish(newProjection(DurableState{ClusterID: "test", LogIndex: 1}), nil)
	s.SetOnApplied(func(a Applied) { updates = append(updates, a) })
	s.publish(s.state, nil)
	s.publish(newProjection(DurableState{ClusterID: "test", LogIndex: 2}), []Batch{{BaseIndex: 1}})
	s.publish(newProjection(DurableState{ClusterID: "test", LogIndex: 1}), nil)
	s.publish(newProjection(DurableState{ClusterID: "test", LogIndex: 3}), []Batch{{BaseIndex: 1}, {BaseIndex: 2}})
	if len(updates) != 3 || !updates[0].Reset || updates[1].Reset || updates[2].Reset {
		t.Fatalf("unexpected publications: %+v", updates)
	}
	if !reflect.DeepEqual(updates[2].Batches, []Batch{{BaseIndex: 2}}) || s.state.LogIndex != 3 {
		t.Fatal("publication repeated an already-applied batch or regressed")
	}
}
