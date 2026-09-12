package delivery

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
	defaultRolloutSchedulingWait = 5 * time.Minute
	defaultHealthcheckTimeout    = 5 * time.Minute
	defaultDrainingTime          = time.Duration(0)
	maxRolloutDeadline           = 24 * time.Hour
)

var ErrVolumeRollingUnsupported = errors.New("volume-backed services cannot overlap rollout generations until volume handoff is supported")

var ErrRolloutInProgress = errors.New("a rollout is already in progress")

func canonicalRollingStrategy(strategy *platformv1.RollingStrategy) *platformv1.RollingStrategy {
	out := &platformv1.RollingStrategy{
		HealthcheckTimeoutSeconds: proto.Int32(int32(defaultHealthcheckTimeout / time.Second)),
		DrainingSeconds:           proto.Int32(int32(defaultDrainingTime / time.Second)),
	}
	if strategy == nil {
		return out
	}
	if strategy.HealthcheckTimeoutSeconds != nil {
		out.HealthcheckTimeoutSeconds = proto.Int32(strategy.GetHealthcheckTimeoutSeconds())
	}
	if strategy.DrainingSeconds != nil {
		out.DrainingSeconds = proto.Int32(strategy.GetDrainingSeconds())
	}
	return out
}

func ValidateRollingStrategy(spec *platformv1.ServiceSpec) error {
	strategy := canonicalRollingStrategy(spec.GetRollingStrategy())
	healthcheckTimeout := time.Duration(strategy.GetHealthcheckTimeoutSeconds()) * time.Second
	if healthcheckTimeout < time.Second || healthcheckTimeout > maxRolloutDeadline {
		return fmt.Errorf("healthcheck timeout must be between 1 second and %s", maxRolloutDeadline)
	}
	drainingTime := time.Duration(strategy.GetDrainingSeconds()) * time.Second
	if drainingTime < 0 || drainingTime > maxRolloutDeadline {
		return fmt.Errorf("draining time must be between 0 seconds and %s", maxRolloutDeadline)
	}
	return nil
}
