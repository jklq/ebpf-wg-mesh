package controlplane

import (
	"errors"
	"fmt"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/proto"
)

const (
	defaultRolloutMaxUnavailable = 0
	defaultRolloutMaxSurge       = 1
	defaultRolloutStartupTimeout = 5 * time.Minute
	defaultRolloutDrainTimeout   = 30 * time.Second
	maxRolloutDeadline           = 24 * time.Hour
)

var errVolumeRollingUnsupported = errors.New("volume-backed services cannot overlap rollout generations until volume handoff is supported")

var errRolloutInProgress = errors.New("a rollout is already in progress")

func canonicalRollingStrategy(strategy *platformv1.RollingStrategy) *platformv1.RollingStrategy {
	out := &platformv1.RollingStrategy{
		MaxUnavailable:        proto.Int32(defaultRolloutMaxUnavailable),
		MaxSurge:              proto.Int32(defaultRolloutMaxSurge),
		StartupTimeoutSeconds: proto.Int32(int32(defaultRolloutStartupTimeout / time.Second)),
		DrainTimeoutSeconds:   proto.Int32(int32(defaultRolloutDrainTimeout / time.Second)),
	}
	if strategy == nil {
		return out
	}
	if strategy.MaxUnavailable != nil {
		out.MaxUnavailable = proto.Int32(strategy.GetMaxUnavailable())
	}
	if strategy.MaxSurge != nil {
		out.MaxSurge = proto.Int32(strategy.GetMaxSurge())
	}
	if strategy.StartupTimeoutSeconds != nil {
		out.StartupTimeoutSeconds = proto.Int32(strategy.GetStartupTimeoutSeconds())
	}
	if strategy.DrainTimeoutSeconds != nil {
		out.DrainTimeoutSeconds = proto.Int32(strategy.GetDrainTimeoutSeconds())
	}
	return out
}

func validateRollingStrategy(spec *platformv1.ServiceSpec) error {
	strategy := canonicalRollingStrategy(spec.GetRollingStrategy())
	desired := specReplicaCount(spec, defaultDesiredReplicaCount)
	if strategy.GetMaxUnavailable() < 0 || strategy.GetMaxUnavailable() > desired {
		return fmt.Errorf("max unavailable must be between 0 and %d", desired)
	}
	if strategy.GetMaxSurge() < 0 || strategy.GetMaxSurge() > maxDesiredReplicaCount {
		return fmt.Errorf("max surge must be between 0 and %d", maxDesiredReplicaCount)
	}
	if strategy.GetMaxUnavailable() == 0 && strategy.GetMaxSurge() == 0 {
		return errors.New("max unavailable and max surge cannot both be zero")
	}
	startup := time.Duration(strategy.GetStartupTimeoutSeconds()) * time.Second
	if startup < time.Second || startup > maxRolloutDeadline {
		return fmt.Errorf("startup timeout must be between 1 second and %s", maxRolloutDeadline)
	}
	drain := time.Duration(strategy.GetDrainTimeoutSeconds()) * time.Second
	if drain < time.Second || drain > maxRolloutDeadline {
		return fmt.Errorf("drain timeout must be between 1 second and %s", maxRolloutDeadline)
	}
	return nil
}
