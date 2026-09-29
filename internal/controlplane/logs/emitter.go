package logs

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/logpipeline"
)

// Platform event names for structured control-plane log lines. Customer output
// never carries these.
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
	// async queues synthetic lines for durable write; nil writes synchronously.
	async *AsyncIngester
}

type Writer interface {
	Enabled() bool
	WriteLogLines(context.Context, []LogLineInput) error
	WriteGaps(context.Context, []GapInput) error
}

// EmitBuildDrops persists builder drop reports as explicit read gaps. Service,
// build, and type come from the resolved scope, not the producer's claim.
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

func NewLogEmitter(store Writer, async *AsyncIngester) *LogEmitter {
	return &LogEmitter{store: store, async: async}
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

// EmitEvent writes one structured platform-event line. Identity and observed_at derive
// from content-stable facts — event, build, lease attempt, event time, never wall-clock
// report times — so retried reports collapse into one row via (observed_at, line_id) dedup.
func (e *LogEmitter) EmitEvent(ctx context.Context, scope ServiceScope, logType LogType, buildID string, leaseEpoch int64, at time.Time, event, line string, attrs map[string]string) {
	stage := StageDeploy
	if logType == LogTypeBuild {
		stage = StageBuild
	}
	at = at.UTC()
	e.emit(ctx, LogLineInput{
		ID: logpipeline.StableEventID(
			event,
			buildID,
			strconv.FormatInt(leaseEpoch, 10),
			at.Format(time.RFC3339Nano),
		),
		ObservedAt:        at,
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

func (e *LogEmitter) emit(_ context.Context, in LogLineInput) {
	if !e.Enabled() {
		return
	}
	if e.async != nil {
		switch e.async.EnqueueLines([]LogLineInput{in}) {
		case AdmitAccepted:
			// Queue the event like an agent batch: retry, shed with gaps, drain at shutdown.
			return
		case AdmitRetry:
			slog.Warn("synthetic log line not journaled",
				"service_id", in.ServiceID,
				"log_type", string(in.LogType),
				"stage", in.Stage,
			)
			return
		case AdmitClosed:
		}
	}
	if err := e.store.WriteLogLines(context.Background(), []LogLineInput{in}); err != nil {
		slog.Warn("write synthetic log line",
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

var synthSequenceCounter atomic.Uint64

func NextSequence() uint64 {
	return synthSequenceCounter.Add(1)
}
