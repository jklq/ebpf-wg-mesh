package delivery

import (
	"strings"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/proto"
)

func TestCanonicalRollingStrategyAppliesTimingDefaults(t *testing.T) {
	t.Parallel()

	got := canonicalRollingStrategy(&platformv1.RollingStrategy{})
	if got.GetHealthcheckTimeoutSeconds() != 300 || got.GetDrainingSeconds() != 0 {
		t.Fatalf("timeout defaults = %+v", got)
	}
}

func TestCanonicalRollingStrategyPreservesExplicitTiming(t *testing.T) {
	t.Parallel()

	got := canonicalRollingStrategy(&platformv1.RollingStrategy{
		HealthcheckTimeoutSeconds: proto.Int32(60),
		DrainingSeconds:           proto.Int32(5),
	})
	if got.GetHealthcheckTimeoutSeconds() != 60 || got.GetDrainingSeconds() != 5 {
		t.Fatalf("strategy = %+v, want healthcheck timeout 60s and draining time 5s", got)
	}
}

func TestValidateRollingStrategyRejectsInvalidHealthcheckTimeout(t *testing.T) {
	t.Parallel()

	spec := &platformv1.ServiceSpec{
		RollingStrategy: &platformv1.RollingStrategy{
			HealthcheckTimeoutSeconds: proto.Int32(0),
		},
	}
	if err := ValidateRollingStrategy(spec); err == nil || !strings.Contains(err.Error(), "healthcheck timeout") {
		t.Fatalf("got %v, want healthcheck timeout error", err)
	}
}
