package logs

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/logpipeline"
)

// Platform event names for structured control-plane log lines.
// Customer output lines never carry these: the pipeline does not
// parse customer output to derive events or attributes.
const (
	EventBuildStarted  = "build.started"
	EventBuildFinished = "build.finished"
	EventDeployStarted = "deploy.started"
	EventCrashLoop     = "allocation.crash_loop"
)

type ServiceScope struct {
	EnvironmentID     string
	ServiceID         string
	RolloutGeneration int64
	AgentID           string
	AllocationID      string
}

type LogEmitter struct {
	store Writer
}

type Writer interface {
	Enabled() bool
	WriteLogLines(context.Context, []LogLineInput) error
	WriteGaps(context.Context, []GapInput) error
}

// EmitBuildDrops persists builder drop reports as explicit read gaps.
// Service, build, and type come from the resolved scope, not the
// producer's claim.
func (e *LogEmitter) EmitBuildDrops(ctx context.Context, scope ServiceScope, buildID, builderID string, drops []*platformv1.LogDropSummary) error {
	if !e.Enabled() || len(drops) == 0 {
		return nil
	}
	gaps := DropSummariesToGaps(drops, builderID)
	for i := range gaps {
		gaps[i].ServiceID = scope.ServiceID
		gaps[i].BuildID = buildID
		gaps[i].LogType = LogTypeBuild
	}
	return e.store.WriteGaps(ctx, gaps)
}

func NewLogEmitter(store Writer) *LogEmitter {
	return &LogEmitter{store: store}
}

func (e *LogEmitter) Enabled() bool {
	return e != nil && e.store != nil && e.store.Enabled()
}

func (e *LogEmitter) EmitBuild(ctx context.Context, scope ServiceScope, buildID, stage, line string) {
	e.emit(ctx, LogLineInput{
		ID:                logpipeline.SyntheticLineID(),
		ObservedAt:        time.Now().UTC(),
		EnvironmentID:     scope.EnvironmentID,
		ServiceID:         scope.ServiceID,
		AllocationID:      "",
		AgentID:           "",
		Stream:            "combined",
		LogType:           LogTypeBuild,
		BuildID:           buildID,
		Stage:             stage,
		RolloutGeneration: scope.RolloutGeneration,
		Sequence:          NextSequence(),
		Line:              strings.TrimRight(line, "\n"),
	})
}

func (e *LogEmitter) EmitBuildf(ctx context.Context, scope ServiceScope, buildID, stage, format string, args ...any) {
	e.EmitBuild(ctx, scope, buildID, stage, fmt.Sprintf(format, args...))
}

// EmitEvent writes one structured platform-event line. Attributes
// describe the known event; the human-readable line stays exact. The
// pipeline never derives events or attributes from customer output.
func (e *LogEmitter) EmitEvent(ctx context.Context, scope ServiceScope, logType LogType, buildID, event, line string, attrs map[string]string) {
	stage := StageDeploy
	if logType == LogTypeBuild {
		stage = StageBuild
	}
	e.emit(ctx, LogLineInput{
		ID:                logpipeline.SyntheticLineID(),
		ObservedAt:        time.Now().UTC(),
		EnvironmentID:     scope.EnvironmentID,
		ServiceID:         scope.ServiceID,
		AllocationID:      scope.AllocationID,
		AgentID:           scope.AgentID,
		Stream:            "combined",
		LogType:           logType,
		BuildID:           buildID,
		Stage:             stage,
		Event:             event,
		Attributes:        attrs,
		RolloutGeneration: scope.RolloutGeneration,
		Sequence:          NextSequence(),
		Line:              strings.TrimRight(line, "\n"),
	})
}

func (e *LogEmitter) EmitBuildLines(ctx context.Context, scope ServiceScope, buildID, builderID string, lines []*platformv1.BuildLogLine) error {
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
			ID:                strings.TrimSpace(line.GetLineId()),
			ObservedAt:        observedAt,
			EnvironmentID:     scope.EnvironmentID,
			ServiceID:         scope.ServiceID,
			AllocationID:      "",
			AgentID:           builderID,
			Stream:            normalizeLogStream(line.GetStream()),
			LogType:           LogTypeBuild,
			BuildID:           buildID,
			Stage:             StageBuild,
			Truncated:         line.GetTruncated(),
			RolloutGeneration: scope.RolloutGeneration,
			Sequence:          line.GetSequence(),
			Line:              text,
		})
	}
	if len(inputs) == 0 {
		return nil
	}
	return e.store.WriteLogLines(ctx, inputs)
}

func (e *LogEmitter) EmitDeploy(ctx context.Context, scope ServiceScope, allocationID, buildID, stage, line string) {
	e.emit(ctx, LogLineInput{
		ID:                logpipeline.SyntheticLineID(),
		ObservedAt:        time.Now().UTC(),
		EnvironmentID:     scope.EnvironmentID,
		ServiceID:         scope.ServiceID,
		AllocationID:      allocationID,
		AgentID:           scope.AgentID,
		Stream:            "combined",
		LogType:           LogTypeDeploy,
		BuildID:           buildID,
		Stage:             stage,
		RolloutGeneration: scope.RolloutGeneration,
		Sequence:          NextSequence(),
		Line:              strings.TrimRight(line, "\n"),
	})
}

func (e *LogEmitter) EmitDeployf(ctx context.Context, scope ServiceScope, allocationID, buildID, stage, format string, args ...any) {
	e.EmitDeploy(ctx, scope, allocationID, buildID, stage, fmt.Sprintf(format, args...))
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

const (
	StageInitialization = "initialization"
	StageBuild          = "build"
	StageDeploy         = "deploy"
	StagePostDeploy     = "post-deploy"
)

var synthSequenceCounter uint64

func NextSequence() uint64 {
	synthSequenceCounter++
	return synthSequenceCounter
}
