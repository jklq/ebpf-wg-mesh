package controlplane

import (
	"strings"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/proto"
)

func TestCanonicalRollingStrategyPreservesExplicitZeroSurge(t *testing.T) {
	t.Parallel()

	got := canonicalRollingStrategy(&platformv1.RollingStrategy{
		MaxUnavailable: proto.Int32(1),
		MaxSurge:       proto.Int32(0),
	})
	if got.GetMaxUnavailable() != 1 || got.GetMaxSurge() != 0 {
		t.Fatalf("strategy = %+v, want unavailable=1 surge=0", got)
	}
	if got.GetStartupTimeoutSeconds() != 300 || got.GetDrainTimeoutSeconds() != 30 {
		t.Fatalf("timeout defaults = %+v", got)
	}
}

func TestValidateRollingStrategyRejectsBothBoundsZero(t *testing.T) {
	t.Parallel()

	spec := &platformv1.ServiceSpec{
		DesiredReplicaCount: proto.Int32(2),
		RollingStrategy: &platformv1.RollingStrategy{
			MaxUnavailable: proto.Int32(0),
			MaxSurge:       proto.Int32(0),
		},
	}
	err := validateRollingStrategy(spec)
	if err == nil || !strings.Contains(err.Error(), "cannot both be zero") {
		t.Fatalf("got %v, want both-zero error", err)
	}
}

func TestValidateRollingStrategyRejectsUnavailableAboveDesired(t *testing.T) {
	t.Parallel()

	spec := &platformv1.ServiceSpec{
		DesiredReplicaCount: proto.Int32(1),
		RollingStrategy: &platformv1.RollingStrategy{
			MaxUnavailable: proto.Int32(2),
			MaxSurge:       proto.Int32(1),
		},
	}
	if err := validateRollingStrategy(spec); err == nil {
		t.Fatal("expected max unavailable to be rejected")
	}
}
