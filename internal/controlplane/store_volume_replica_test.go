package controlplane

import (
	"errors"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func TestValidateVolumeReplicaCompatibility(t *testing.T) {
	t.Parallel()

	volumeSpec := &platformv1.ServiceSpec{
		Runtime: &platformv1.ServiceRuntime{VolumeName: "data"},
	}
	if err := validateVolumeReplicaCompatibility(volumeSpec, 1); err != nil {
		t.Fatalf("volume with 1 replica should be allowed: %v", err)
	}
	if err := validateVolumeReplicaCompatibility(nil, 3); err != nil {
		t.Fatalf("replicas without a volume should be allowed: %v", err)
	}
	err := validateVolumeReplicaCompatibility(volumeSpec, 2)
	if !errors.Is(err, errVolumeReplicaUnsupported) {
		t.Fatalf("expected errVolumeReplicaUnsupported, got %v", err)
	}
}
