package logs

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

type ServiceScope struct {
	EnvironmentID     string
	ServiceID         string
	RolloutGeneration int64
	AgentID           string
}

type LogEmitter struct {
	store Writer
}

type Writer interface {
	Enabled() bool
	WriteLogLines(context.Context, []LogLineInput) error
}

func NewLogEmitter(store Writer) *LogEmitter {
	return &LogEmitter{store: store}
}

func (e *LogEmitter) Enabled() bool {
	return e != nil && e.store != nil && e.store.Enabled()
}

func (e *LogEmitter) EmitBuild(ctx context.Context, scope ServiceScope, buildID, stage, line string) {
	e.emit(ctx, LogLineInput{
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
			ObservedAt:        observedAt,
			EnvironmentID:     scope.EnvironmentID,
			ServiceID:         scope.ServiceID,
			AllocationID:      "",
			AgentID:           builderID,
			Stream:            normalizeLogStream(line.GetStream()),
			LogType:           LogTypeBuild,
			BuildID:           buildID,
			Stage:             StageBuild,
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
