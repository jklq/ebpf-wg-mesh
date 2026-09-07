package controlplane

import (
	"context"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"fmt"
	"log/slog"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

// LogEmitter is an internal helper used by the control plane to persist
// synthetic log lines for deployment events (build lifecycle, scheduling,
// allocation transitions, …). It keeps the ClickHouse sink as the single
// source of truth for every log stream the UI renders, so the frontend can
// treat build, deploy and runtime logs uniformly.
//
// We deliberately write synchronously - these are low-volume events (single
// digit writes per deploy) and losing them would degrade the UX in an
// observable way.
type LogEmitter struct {
	store interface {
		Enabled() bool
		WriteLogLines(ctx context.Context, inputs []LogLineInput) error
	}
}

// NewLogEmitter wires a LogStore into the emitter. A nil store is a valid
// no-op, which lets callers not care whether log capture is actually
// configured.
func NewLogEmitter(store *LogStore) *LogEmitter {
	return &LogEmitter{store: store}
}

// Enabled returns true if the underlying sink will accept writes.
func (e *LogEmitter) Enabled() bool {
	return e != nil && e.store != nil && e.store.Enabled()
}

// EmitBuild persists a build log line keyed to a specific build run. Lines are
// grouped by stage so the UI can fold them into the stage timeline view.
func (e *LogEmitter) EmitBuild(ctx context.Context, service deliverycore.ServiceRecord, build deliverycore.BuildRunRecord, stage, line string) {
	e.emit(ctx, LogLineInput{
		ObservedAt:        time.Now().UTC(),
		EnvironmentID:     service.EnvironmentID,
		ServiceID:         service.ID,
		AllocationID:      "",
		AgentID:           "",
		Stream:            "combined",
		LogType:           LogTypeBuild,
		BuildID:           build.ID,
		Stage:             stage,
		RolloutGeneration: service.RolloutGeneration,
		Sequence:          nextSynthSequence(),
		Line:              strings.TrimRight(line, "\n"),
	})
}

// EmitBuildf is a printf variant for EmitBuild.
func (e *LogEmitter) EmitBuildf(ctx context.Context, service deliverycore.ServiceRecord, build deliverycore.BuildRunRecord, stage, format string, args ...any) {
	e.EmitBuild(ctx, service, build, stage, fmt.Sprintf(format, args...))
}

// EmitBuildLines persists raw build stdout/stderr lines reported live by the
// builder. The service/build records provide the trusted project/service
// identity so builders cannot spoof log ownership.
func (e *LogEmitter) EmitBuildLines(ctx context.Context, service deliverycore.ServiceRecord, build deliverycore.BuildRunRecord, builderID string, lines []*platformv1.BuildLogLine) error {
	if !e.Enabled() || len(lines) == 0 {
		return nil
	}
	now := time.Now().UTC()
	inputs := make([]LogLineInput, 0, len(lines))
	for _, line := range lines {
		if line == nil {
			continue
		}
		text := strings.TrimRight(line.GetLine(), "\r\n")
		if text == "" {
			continue
		}
		observedAt := line.GetObservedAt().AsTime().UTC()
		if observedAt.IsZero() {
			observedAt = now
		}
		inputs = append(inputs, LogLineInput{
			ObservedAt:        observedAt,
			EnvironmentID:     service.EnvironmentID,
			ServiceID:         service.ID,
			AllocationID:      "",
			AgentID:           builderID,
			Stream:            normalizeLogStream(line.GetStream()),
			LogType:           LogTypeBuild,
			BuildID:           build.ID,
			Stage:             StageBuild,
			RolloutGeneration: service.RolloutGeneration,
			Sequence:          line.GetSequence(),
			Line:              text,
		})
	}
	if len(inputs) == 0 {
		return nil
	}
	return e.store.WriteLogLines(ctx, inputs)
}

// EmitDeploy persists a deploy log line attached to the current rollout
// generation. Callers supply an allocation id when available so the UI can
// correlate with runtime logs; during initial scheduling it may be empty.
func (e *LogEmitter) EmitDeploy(ctx context.Context, service deliverycore.ServiceRecord, allocationID, buildID, stage, line string) {
	e.emit(ctx, LogLineInput{
		ObservedAt:        time.Now().UTC(),
		EnvironmentID:     service.EnvironmentID,
		ServiceID:         service.ID,
		AllocationID:      allocationID,
		AgentID:           service.AllocatedAgentID,
		Stream:            "combined",
		LogType:           LogTypeDeploy,
		BuildID:           buildID,
		Stage:             stage,
		RolloutGeneration: service.RolloutGeneration,
		Sequence:          nextSynthSequence(),
		Line:              strings.TrimRight(line, "\n"),
	})
}

// EmitDeployf is a printf variant for EmitDeploy.
func (e *LogEmitter) EmitDeployf(ctx context.Context, service deliverycore.ServiceRecord, allocationID, buildID, stage, format string, args ...any) {
	e.EmitDeploy(ctx, service, allocationID, buildID, stage, fmt.Sprintf(format, args...))
}

func (e *LogEmitter) emit(ctx context.Context, in LogLineInput) {
	if !e.Enabled() {
		return
	}
	if err := e.store.WriteLogLines(ctx, []LogLineInput{in}); err != nil {
		slog.WarnContext(ctx, "write synthetic log line",
			"error", err,
			"service_id", in.ServiceID,
			"log_type", string(in.LogType),
			"stage", in.Stage,
		)
	}
}

// Stage identifiers. Keeping these as constants means both the writer and the
// stage-projection logic agree on the shape of the deployment timeline without
// a shared config file.
const (
	StageInitialization = "initialization"
	StageBuild          = "build"
	StageDeploy         = "deploy"
	StagePostDeploy     = "post-deploy"
)

// synthSequence increments a monotonically-increasing counter used as the
// per-service sequence for synthetic logs. We keep it process-wide rather than
// per-service because these rows are low-volume and ordering is primarily a
// UI tiebreaker — observed_at is still authoritative.
var synthSequenceCounter uint64

func nextSynthSequence() uint64 {
	synthSequenceCounter++
	return synthSequenceCounter
}
