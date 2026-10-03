package delivery

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/authz"
	"ebof-wg-mesh/internal/controlplane/dbtx"
	"ebof-wg-mesh/internal/controlplane/source"

	"github.com/google/uuid"
)

// BuildSchedulerConfig tunes build leases and queue limits.
type BuildSchedulerConfig struct {
	// LeaseTTL is how long a claimed build stays owned without a heartbeat.
	LeaseTTL time.Duration
	// AttemptLimit bounds total claims including takeovers.
	AttemptLimit int64
	// MaxConcurrentGlobal caps running builds cluster-wide.
	MaxConcurrentGlobal int
	// MaxConcurrentPerProject caps running builds per project.
	MaxConcurrentPerProject int
	// BuildTimeout bounds one claim from claim to completion.
	BuildTimeout time.Duration
	// MaxQueueAge bounds how long a build may wait in queued state.
	MaxQueueAge time.Duration
}

func DefaultBuildSchedulerConfig() BuildSchedulerConfig {
	defaults := config.DefaultControlPlaneBuilderConfig()
	return BuildSchedulerConfig{
		LeaseTTL:                time.Duration(defaults.LeaseTTLSeconds) * time.Second,
		AttemptLimit:            int64(defaults.MaxAttempts),
		MaxConcurrentGlobal:     defaults.MaxConcurrentGlobal,
		MaxConcurrentPerProject: defaults.MaxConcurrentPerProject,
		BuildTimeout:            time.Duration(defaults.BuildTimeoutSeconds) * time.Second,
		MaxQueueAge:             time.Duration(defaults.MaxQueueAgeSeconds) * time.Second,
	}
}

func (c BuildSchedulerConfig) WithDefaults() BuildSchedulerConfig {
	def := DefaultBuildSchedulerConfig()
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = def.LeaseTTL
	}
	if c.AttemptLimit <= 0 {
		c.AttemptLimit = def.AttemptLimit
	}
	if c.MaxConcurrentGlobal <= 0 {
		c.MaxConcurrentGlobal = def.MaxConcurrentGlobal
	}
	if c.MaxConcurrentPerProject <= 0 {
		c.MaxConcurrentPerProject = def.MaxConcurrentPerProject
	}
	if c.BuildTimeout <= 0 {
		c.BuildTimeout = def.BuildTimeout
	}
	if c.MaxQueueAge <= 0 {
		c.MaxQueueAge = def.MaxQueueAge
	}
	return c
}

func scanBuildRunRow(scanner interface{ Scan(...any) error }) (BuildRunRecord, error) {
	var (
		rec        BuildRunRecord
		recipeJSON []byte
	)
	if err := scanner.Scan(
		&rec.ID,
		&rec.ServiceID,
		&rec.CommitSHA,
		&rec.CommitMessage,
		&rec.CommitContributors,
		&rec.State,
		&rec.BuilderID,
		&rec.OwnerEpoch,
		&rec.LeaseExpiresAt,
		&rec.AttemptCount,
		&rec.AttemptLimit,
		&rec.CancelRequestedAt,
		&rec.DeadlineAt,
		&rec.LastHeartbeatAt,
		&rec.ArtifactID,
		&rec.ImageDigest,
		&rec.FailureReason,
		&rec.SourceRevisionID,
		&rec.SourceSnapshotID,
		&rec.SourceSnapshotDigest,
		&rec.BuildActorKind,
		&rec.BuildActorID,
		&rec.TargetRolloutGeneration,
		&recipeJSON,
		&rec.QueuedAt,
		&rec.StartedAt,
		&rec.FinishedAt,
	); err != nil {
		return BuildRunRecord{}, err
	}
	var err error
	rec.BuildRecipe, err = source.UnmarshalBuildRecipe(recipeJSON)
	if err != nil {
		return BuildRunRecord{}, err
	}
	return rec, nil
}

func scanBuildAttemptRow(scanner interface{ Scan(...any) error }) (BuildAttemptRecord, error) {
	var rec BuildAttemptRecord
	err := scanner.Scan(
		&rec.AttemptNumber,
		&rec.BuilderID,
		&rec.OwnerEpoch,
		&rec.StartedAt,
		&rec.FinishedAt,
		&rec.Outcome,
		&rec.Detail,
	)
	return rec, err
}

func (d *Delivery) ClaimNextBuild(ctx context.Context, builderID, builderName string) (BuildRunRecord, error) {
	s := d.store
	scheduler := d.buildScheduler
	var rec BuildRunRecord
	err := s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rec = BuildRunRecord{}
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO builder_workers(id, name, current_build_id, last_heartbeat_at, created_at, updated_at, drained)
			 VALUES ($1, $2, '', $3, $3, $3, FALSE)
			 ON CONFLICT(id) DO UPDATE
			    SET name = excluded.name,
			        last_heartbeat_at = excluded.last_heartbeat_at,
			        updated_at = excluded.updated_at`,
			builderID, builderName, now,
		); err != nil {
			return err
		}
		if err := d.recoverExpiredBuildsTx(ctx, tx, now, scheduler); err != nil {
			return err
		}
		// Resuming an owned running build is not a new claim: a drained builder or paused
		// scheduler still hands back its own work so running builds finish.
		existing, err := scanBuildRunRow(tx.QueryRowContext(ctx, `SELECT `+buildRunSelectColumns+` FROM build_runs WHERE state = $1 AND builder_id = $2 ORDER BY started_at, id LIMIT 1`, BuildStateRunning, builderID))
		if err == nil {
			rec, err = s.buildRunByIDQuerier(ctx, tx, existing.ID)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var drained bool
		if err := tx.QueryRowContext(ctx, `SELECT drained FROM builder_workers WHERE id = $1`, builderID).Scan(&drained); err != nil {
			return err
		}
		if drained {
			return nil
		}
		var paused bool
		if err := tx.QueryRowContext(ctx, `SELECT paused FROM build_scheduler_control WHERE id = TRUE`).Scan(&paused); err != nil {
			return err
		}
		if paused {
			return nil
		}
		var globalRunning int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM build_runs WHERE state = $1`, BuildStateRunning).Scan(&globalRunning); err != nil {
			return err
		}
		if globalRunning >= scheduler.MaxConcurrentGlobal {
			return nil
		}
		candidates, err := listClaimCandidatesTx(ctx, tx, 20)
		if err != nil {
			return err
		}
		for _, candidateID := range candidates {
			claimed, ok, err := d.tryClaimBuildTx(ctx, tx, candidateID, builderID, now, scheduler)
			if err != nil {
				return err
			}
			if ok {
				rec = claimed
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return BuildRunRecord{}, err
	}
	return rec, nil
}

func listClaimCandidatesTx(ctx context.Context, tx *sql.Tx, limit int) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT b.id
		   FROM build_runs b
		  WHERE b.state = $1
		    AND NOT EXISTS (
		        SELECT 1 FROM services s
		         JOIN environments e ON e.id = s.environment_id
		         JOIN projects p ON p.id = e.project_id
		        WHERE s.id = b.service_id
		          AND (s.deleted_at IS NOT NULL OR e.deleted_at IS NOT NULL OR p.deleted_at IS NOT NULL)
		    )
		  ORDER BY b.queued_at ASC, b.id ASC
		  LIMIT $2`,
		BuildStateQueued, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (d *Delivery) tryClaimBuildTx(ctx context.Context, tx *sql.Tx, buildID, builderID string, now time.Time, scheduler BuildSchedulerConfig) (BuildRunRecord, bool, error) {
	s := d.store
	serviceID, err := serviceIDForBuildTx(ctx, tx, buildID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return BuildRunRecord{}, false, nil
		}
		return BuildRunRecord{}, false, err
	}
	if err := s.lockServiceTx(ctx, tx, serviceID); err != nil {
		return BuildRunRecord{}, false, err
	}
	build, err := s.buildRunByIDQuerier(ctx, tx, buildID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return BuildRunRecord{}, false, nil
		}
		return BuildRunRecord{}, false, err
	}
	if build.State != BuildStateQueued {
		return BuildRunRecord{}, false, nil
	}
	if deletion, err := s.serviceDeletionQuerier(ctx, tx, build.ServiceID); err != nil {
		return BuildRunRecord{}, false, err
	} else if deletion != nil {
		if _, err := tx.ExecContext(ctx,
			`UPDATE build_runs SET state = $1, failure_reason = $2, finished_at = $3 WHERE id = $4 AND state = $5`,
			BuildStateCancelled, "service deleted", now, buildID, BuildStateQueued,
		); err != nil {
			return BuildRunRecord{}, false, err
		}
		return BuildRunRecord{}, false, nil
	}
	dep, ok, err := s.deploymentByBuildIDTx(ctx, tx, build.ServiceID, build.ID)
	if err != nil {
		return BuildRunRecord{}, false, err
	}
	if ok && (deploymentStateTerminal(dep.State) || !dep.IsCurrent) {
		return BuildRunRecord{}, false, nil
	}
	var projectRunning int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*)
		   FROM build_runs b
		   JOIN services s ON s.id = b.service_id
		   JOIN environments e ON e.id = s.environment_id
		  WHERE b.state = $1 AND e.project_id = $2`,
		BuildStateRunning, build.ProjectID,
	).Scan(&projectRunning); err != nil {
		return BuildRunRecord{}, false, err
	}
	if projectRunning >= scheduler.MaxConcurrentPerProject {
		return BuildRunRecord{}, false, nil
	}
	leaseExpires := now.Add(scheduler.LeaseTTL)
	deadline := now.Add(scheduler.BuildTimeout)
	result, err := tx.ExecContext(ctx,
		`UPDATE build_runs
		    SET state = $1,
		        started_at = $2,
		        builder_id = $3,
		        owner_epoch = owner_epoch + 1,
		        attempt_count = attempt_count + 1,
		        lease_expires_at = $4,
		        deadline_at = $5,
		        last_heartbeat_at = $2
		  WHERE id = $6 AND state = $7`,
		BuildStateRunning, now, builderID, leaseExpires, deadline, buildID, BuildStateQueued,
	)
	if err != nil {
		return BuildRunRecord{}, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return BuildRunRecord{}, false, err
	}
	if affected != 1 {
		return BuildRunRecord{}, false, nil
	}
	claimed, err := s.buildRunByIDQuerier(ctx, tx, buildID)
	if err != nil {
		return BuildRunRecord{}, false, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO build_attempts(id, build_id, attempt_number, builder_id, owner_epoch, started_at, outcome)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		uuid.NewString(), buildID, claimed.AttemptCount, builderID, claimed.OwnerEpoch, now, BuildAttemptLeased,
	); err != nil {
		return BuildRunRecord{}, false, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE builder_workers
		    SET current_build_id = $1,
		        last_heartbeat_at = $2,
		        updated_at = $2
		  WHERE id = $3`,
		buildID, now, builderID,
	); err != nil {
		return BuildRunRecord{}, false, err
	}
	if _, err := s.applyDeploymentTransitionByBuildTx(ctx, tx, claimed.ServiceID, claimed.ID, deploymentTransitionInput{
		ToState:          DeploymentStateBuilding,
		Actor:            deploymentActor{Kind: DeploymentCauseBuilder, ID: builderID},
		ReasonCode:       reasonBuildStarted,
		Detail:           "Builder claimed the build",
		IgnoreIfTerminal: true,
	}); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return BuildRunRecord{}, false, err
	}
	return claimed, true, nil
}

func serviceIDForBuildTx(ctx context.Context, tx *sql.Tx, buildID string) (string, error) {
	var serviceID string
	if err := tx.QueryRowContext(ctx, `SELECT service_id FROM build_runs WHERE id = $1`, buildID).Scan(&serviceID); err != nil {
		return "", err
	}
	return serviceID, nil
}

func (d *Delivery) RecoverExpiredBuilds(ctx context.Context) error {
	scheduler := d.buildScheduler
	return d.store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		return d.recoverExpiredBuildsTx(ctx, tx, now, scheduler)
	})
}

func (d *Delivery) recoverExpiredBuildsTx(ctx context.Context, tx *sql.Tx, now time.Time, scheduler BuildSchedulerConfig) error {
	if err := d.expireQueuedBuildsTx(ctx, tx, now, scheduler); err != nil {
		return err
	}
	if err := d.timeoutRunningBuildsTx(ctx, tx, now); err != nil {
		return err
	}
	if err := d.requeueLeaseExpiredBuildsTx(ctx, tx, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE builder_workers w
		    SET current_build_id = '',
		        updated_at = $1
		  WHERE w.current_build_id <> ''
		    AND NOT EXISTS (
		        SELECT 1 FROM build_runs b
		         WHERE b.id = w.current_build_id
		           AND b.state = $2
		           AND b.builder_id = w.id
		    )`,
		now, BuildStateRunning,
	); err != nil {
		return err
	}
	return nil
}

func (d *Delivery) expireQueuedBuildsTx(ctx context.Context, tx *sql.Tx, now time.Time, scheduler BuildSchedulerConfig) error {
	s := d.store
	cutoff := now.Add(-scheduler.MaxQueueAge)
	rows, err := tx.QueryContext(ctx,
		`SELECT id, service_id FROM build_runs WHERE state = $1 AND queued_at < $2 ORDER BY queued_at ASC, id ASC LIMIT 100`,
		BuildStateQueued, cutoff,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	type queued struct{ ID, ServiceID string }
	var expired []queued
	for rows.Next() {
		var rec queued
		if err := rows.Scan(&rec.ID, &rec.ServiceID); err != nil {
			return err
		}
		expired = append(expired, rec)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, rec := range expired {
		if _, err := tx.ExecContext(ctx,
			`UPDATE build_runs SET state = $1, failure_reason = $2, finished_at = $3
			  WHERE id = $4 AND state = $5`,
			BuildStateFailed, "build waited longer than the maximum queue age", now, rec.ID, BuildStateQueued,
		); err != nil {
			return err
		}
		if _, err := s.applyDeploymentTransitionByBuildTx(ctx, tx, rec.ServiceID, rec.ID, deploymentTransitionInput{
			ToState: DeploymentStateFailed, Actor: deploymentActor{Kind: DeploymentCauseSystem},
			ReasonCode: reasonBuildFailed, Detail: "Build expired in queue",
			IgnoreIfTerminal: true,
		}); err != nil && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, errDeploymentTerminal) && !errors.Is(err, errIllegalDeploymentTransition) {
			return err
		}
	}
	return nil
}

// timeoutRunningBuildsTx fails builds past their claim deadline. A timeout is terminal
// even with retry budget left: overruns don't converge by retrying, so retry is manual.
func (d *Delivery) timeoutRunningBuildsTx(ctx context.Context, tx *sql.Tx, now time.Time) error {
	s := d.store
	rows, err := tx.QueryContext(ctx,
		`SELECT id, service_id, attempt_count, cancel_requested_at IS NOT NULL FROM build_runs
		  WHERE state = $1 AND deadline_at IS NOT NULL AND deadline_at <= $2
		  ORDER BY deadline_at ASC, id ASC LIMIT 100`,
		BuildStateRunning, now,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	type timedOut struct {
		ID           string
		ServiceID    string
		AttemptCount int64
		Cancelled    bool
	}
	var expired []timedOut
	for rows.Next() {
		var rec timedOut
		if err := rows.Scan(&rec.ID, &rec.ServiceID, &rec.AttemptCount, &rec.Cancelled); err != nil {
			return err
		}
		expired = append(expired, rec)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, rec := range expired {
		state, reason, outcome := BuildStateFailed, "build timeout exceeded", BuildAttemptTimedOut
		toState, reasonCode, detail := DeploymentStateFailed, reasonBuildFailed, "Build timeout exceeded"
		if rec.Cancelled {
			state, reason, outcome = BuildStateCancelled, "cancelled by user", BuildAttemptCancelled
			toState, reasonCode, detail = DeploymentStateCancelled, reasonUserCancel, "Build cancelled by user"
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE build_runs SET state = $1, failure_reason = $2, finished_at = $3, lease_expires_at = NULL
			  WHERE id = $4 AND state = $5`,
			state, reason, now, rec.ID, BuildStateRunning,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE build_attempts SET finished_at = $1, outcome = $2, detail = $3
			  WHERE build_id = $4 AND attempt_number = $5`,
			now, outcome, reason, rec.ID, rec.AttemptCount,
		); err != nil {
			return err
		}
		if _, err := s.applyDeploymentTransitionByBuildTx(ctx, tx, rec.ServiceID, rec.ID, deploymentTransitionInput{
			ToState: toState, Actor: deploymentActor{Kind: DeploymentCauseSystem},
			ReasonCode: reasonCode, Detail: detail,
			IgnoreIfTerminal: true,
		}); err != nil && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, errDeploymentTerminal) && !errors.Is(err, errIllegalDeploymentTransition) {
			return err
		}
	}
	return nil
}

func (d *Delivery) requeueLeaseExpiredBuildsTx(ctx context.Context, tx *sql.Tx, now time.Time) error {
	s := d.store
	rows, err := tx.QueryContext(ctx,
		`SELECT id, service_id, queued_at, attempt_count, attempt_limit, cancel_requested_at IS NOT NULL
		   FROM build_runs
		  WHERE state = $1 AND lease_expires_at IS NOT NULL AND lease_expires_at <= $2
		  ORDER BY queued_at ASC, id ASC LIMIT 100`,
		BuildStateRunning, now,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	type expiredBuild struct {
		ID              string
		ServiceID       string
		QueuedAt        time.Time
		AttemptCount    int64
		AttemptLimit    int64
		CancelRequested bool
	}
	var expired []expiredBuild
	for rows.Next() {
		var rec expiredBuild
		if err := rows.Scan(&rec.ID, &rec.ServiceID, &rec.QueuedAt, &rec.AttemptCount, &rec.AttemptLimit, &rec.CancelRequested); err != nil {
			return err
		}
		expired = append(expired, rec)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, rec := range expired {
		if rec.CancelRequested {
			if _, err := tx.ExecContext(ctx,
				`UPDATE build_runs SET state = $1, failure_reason = $2, finished_at = $3,
				        builder_id = NULL, lease_expires_at = NULL
				  WHERE id = $4 AND state = $5`,
				BuildStateCancelled, "cancelled by user", now, rec.ID, BuildStateRunning,
			); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE build_attempts SET finished_at = $1, outcome = $2, detail = $3
				  WHERE build_id = $4 AND attempt_number = $5`,
				now, BuildAttemptCancelled, "cancelled while worker was lost", rec.ID, rec.AttemptCount,
			); err != nil {
				return err
			}
			continue
		}
		var newerCount int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*)
			   FROM build_runs
			  WHERE service_id = $1
			    AND queued_at > $2
			    AND state IN ($3, $4, $5)`,
			rec.ServiceID, rec.QueuedAt, BuildStateQueued, BuildStateRunning, BuildStateSucceeded,
		).Scan(&newerCount); err != nil {
			return err
		}
		if newerCount > 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE build_runs SET state = $1, finished_at = $2, builder_id = NULL,
				        lease_expires_at = NULL, failure_reason = $3
				  WHERE id = $4 AND state = $5`,
				BuildStateSuperseded, now, "superseded after worker loss", rec.ID, BuildStateRunning,
			); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE build_attempts SET finished_at = $1, outcome = $2, detail = $3
				  WHERE build_id = $4 AND attempt_number = $5`,
				now, BuildAttemptSuperseded, "superseded after worker loss", rec.ID, rec.AttemptCount,
			); err != nil {
				return err
			}
			if _, err := s.applyDeploymentTransitionByBuildTx(ctx, tx, rec.ServiceID, rec.ID, deploymentTransitionInput{
				ToState: DeploymentStateSuperseded, Actor: deploymentActor{Kind: DeploymentCauseSystem},
				ReasonCode: reasonBuildSuperseded, Detail: "Superseded after worker loss",
				IgnoreIfTerminal: true,
			}); err != nil && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, errDeploymentTerminal) && !errors.Is(err, errIllegalDeploymentTransition) {
				return err
			}
			continue
		}
		if rec.AttemptCount >= rec.AttemptLimit {
			if _, err := tx.ExecContext(ctx,
				`UPDATE build_runs SET state = $1, finished_at = $2, builder_id = NULL,
				        lease_expires_at = NULL, failure_reason = $3
				  WHERE id = $4 AND state = $5`,
				BuildStateFailed, now, "retry budget exhausted after worker loss", rec.ID, BuildStateRunning,
			); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE build_attempts SET finished_at = $1, outcome = $2, detail = $3
				  WHERE build_id = $4 AND attempt_number = $5`,
				now, BuildAttemptWorkerLost, "retry budget exhausted", rec.ID, rec.AttemptCount,
			); err != nil {
				return err
			}
			if _, err := s.applyDeploymentTransitionByBuildTx(ctx, tx, rec.ServiceID, rec.ID, deploymentTransitionInput{
				ToState: DeploymentStateFailed, Actor: deploymentActor{Kind: DeploymentCauseSystem},
				ReasonCode: reasonBuildFailed, Detail: "Build retry budget exhausted after worker loss",
				IgnoreIfTerminal: true,
			}); err != nil && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, errDeploymentTerminal) && !errors.Is(err, errIllegalDeploymentTransition) {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE build_runs
			    SET state = $1,
			        started_at = NULL,
			        builder_id = NULL,
			        lease_expires_at = NULL,
			        deadline_at = NULL,
			        last_heartbeat_at = NULL,
			        failure_reason = ''
			  WHERE id = $2 AND state = $3`,
			BuildStateQueued, rec.ID, BuildStateRunning,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE build_attempts SET finished_at = $1, outcome = $2, detail = $3
			  WHERE build_id = $4 AND attempt_number = $5`,
			now, BuildAttemptWorkerLost, "worker lease expired; build requeued", rec.ID, rec.AttemptCount,
		); err != nil {
			return err
		}
		if _, err := s.applyDeploymentTransitionByBuildTx(ctx, tx, rec.ServiceID, rec.ID, deploymentTransitionInput{
			ToState:          DeploymentStateQueuedBuild,
			Actor:            deploymentActor{Kind: DeploymentCauseSystem},
			ReasonCode:       reasonBuildRequeued,
			Detail:           "Builder lease expired; build requeued",
			IgnoreIfTerminal: true,
		}); err != nil && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, errDeploymentTerminal) && !errors.Is(err, errIllegalDeploymentTransition) {
			return err
		}
	}
	return nil
}

// HeartbeatBuild extends the lease on a running build via compare-and-swap on
// (builder_id, owner_epoch). A superseded owner gets ErrBuildLeaseLost; a build with
// cancellation requested gets ErrBuildCancelled so the builder stops cooperatively.
func (d *Delivery) HeartbeatBuild(ctx context.Context, builderID, buildID string, epoch int64) error {
	scheduler := d.buildScheduler
	return d.store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		var cancelRequested bool
		if err := tx.QueryRowContext(ctx,
			`SELECT cancel_requested_at IS NOT NULL FROM build_runs WHERE id = $1 FOR UPDATE`, buildID,
		).Scan(&cancelRequested); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrBuildNotOwned
			}
			return err
		}
		if cancelRequested {
			return ErrBuildCancelled
		}
		result, err := tx.ExecContext(ctx,
			`UPDATE build_runs
			    SET lease_expires_at = $1,
			        last_heartbeat_at = $2
			  WHERE id = $3 AND state = $4 AND builder_id = $5 AND owner_epoch = $6`,
			now.Add(scheduler.LeaseTTL), now, buildID, BuildStateRunning, builderID, epoch,
		)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return ErrBuildLeaseLost
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE builder_workers SET current_build_id = $1, last_heartbeat_at = $2, updated_at = $2
			  WHERE id = $3`,
			buildID, now, builderID,
		); err != nil {
			return err
		}
		return nil
	})
}

// BuildAttempts returns the attempt history for one build, oldest first.
func (d *Delivery) BuildAttempts(ctx context.Context, user authz.User, serviceID, buildID string) ([]BuildAttemptRecord, error) {
	scope, err := d.store.authz.AuthorizeService(ctx, user, serviceID, authz.Read)
	if err != nil {
		return nil, err
	}
	var serviceOfBuild string
	if err := d.store.db.QueryRowContext(ctx, `SELECT service_id FROM build_runs WHERE id = $1`, buildID).Scan(&serviceOfBuild); err != nil {
		return nil, err
	}
	if serviceOfBuild != scope.ID() {
		return nil, sql.ErrNoRows
	}
	rows, err := d.store.db.QueryContext(ctx,
		`SELECT attempt_number, builder_id, owner_epoch, started_at, finished_at, outcome, detail
		   FROM build_attempts WHERE build_id = $1 ORDER BY attempt_number ASC, id ASC`, buildID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BuildAttemptRecord
	for rows.Next() {
		rec, err := scanBuildAttemptRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// ListBuilders returns builder worker records. Operators only.
func (d *Delivery) ListBuilders(ctx context.Context, user authz.User) ([]BuilderWorkerRecord, error) {
	if _, err := d.store.authz.AuthorizeOperator(ctx, user); err != nil {
		return nil, err
	}
	rows, err := d.store.db.QueryContext(ctx,
		`SELECT id, name, current_build_id, last_heartbeat_at, drained, updated_at FROM builder_workers ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BuilderWorkerRecord
	for rows.Next() {
		var rec BuilderWorkerRecord
		if err := rows.Scan(&rec.ID, &rec.Name, &rec.CurrentBuildID, &rec.LastHeartbeat, &rec.Drained, &rec.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// SetBuilderDrain marks a builder drained (no new claims; running work
// finishes) or undrained. Operators only.
func (d *Delivery) SetBuilderDrain(ctx context.Context, user authz.User, builderID string, drained bool) (BuilderWorkerRecord, error) {
	if _, err := d.store.authz.AuthorizeOperator(ctx, user); err != nil {
		return BuilderWorkerRecord{}, err
	}
	builderID = strings.TrimSpace(builderID)
	if builderID == "" {
		return BuilderWorkerRecord{}, errors.New("builder id is required")
	}
	var rec BuilderWorkerRecord
	err := d.store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx,
			`UPDATE builder_workers SET drained = $1, updated_at = $2 WHERE id = $3`,
			drained, now, builderID,
		)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return sql.ErrNoRows
		}
		return tx.QueryRowContext(ctx,
			`SELECT id, name, current_build_id, last_heartbeat_at, drained, updated_at FROM builder_workers WHERE id = $1`,
			builderID,
		).Scan(&rec.ID, &rec.Name, &rec.CurrentBuildID, &rec.LastHeartbeat, &rec.Drained, &rec.UpdatedAt)
	})
	return rec, err
}

// BuildSchedulerState returns the pause flag plus queue counters. Operators only.
func (d *Delivery) BuildSchedulerState(ctx context.Context, user authz.User) (BuildSchedulerState, error) {
	if _, err := d.store.authz.AuthorizeOperator(ctx, user); err != nil {
		return BuildSchedulerState{}, err
	}
	scheduler := d.buildScheduler
	var state BuildSchedulerState
	state.MaxConcurrentGlobal = scheduler.MaxConcurrentGlobal
	state.MaxConcurrentPerProject = scheduler.MaxConcurrentPerProject
	if err := d.store.db.QueryRowContext(ctx, `SELECT paused, updated_at FROM build_scheduler_control WHERE id = TRUE`).Scan(&state.Paused, &state.UpdatedAt); err != nil {
		return BuildSchedulerState{}, err
	}
	if err := d.store.db.QueryRowContext(ctx,
		`SELECT count(*) FILTER (WHERE state = $1), count(*) FILTER (WHERE state = $2) FROM build_runs`,
		BuildStateRunning, BuildStateQueued,
	).Scan(&state.RunningBuilds, &state.QueuedBuilds); err != nil {
		return BuildSchedulerState{}, err
	}
	return state, nil
}

// SetBuildSchedulerPaused pauses (no new claims; running work finishes) or
// resumes the build queue. Operators only.
func (d *Delivery) SetBuildSchedulerPaused(ctx context.Context, user authz.User, paused bool) (BuildSchedulerState, error) {
	if _, err := d.store.authz.AuthorizeOperator(ctx, user); err != nil {
		return BuildSchedulerState{}, err
	}
	err := d.store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE build_scheduler_control SET paused = $1, updated_at = $2 WHERE id = TRUE`, paused, now)
		return err
	})
	if err != nil {
		return BuildSchedulerState{}, err
	}
	return d.BuildSchedulerState(ctx, user)
}

const (
	BuildStateQueued     = "queued"
	BuildStateRunning    = "running"
	BuildStateSucceeded  = "succeeded"
	BuildStateFailed     = "failed"
	BuildStateSuperseded = "superseded"
	BuildStateCancelled  = "cancelled"
)

var errServiceNotBuildable = errors.New("service does not use a build source")

var errSourceStateNotReady = errors.New("source state is not ready")

var ErrBuildNotOwned = errors.New("build is not assigned to builder")

var ErrBuildLeaseLost = errors.New("build lease lost: owner epoch is stale")

var ErrBuildCancelled = errors.New("build cancellation was requested")

const (
	BuildAttemptLeased         = "leased"
	BuildAttemptSucceeded      = "succeeded"
	BuildAttemptFailedTerminal = "failed_terminal"
	BuildAttemptWorkerLost     = "worker_lost"
	BuildAttemptCancelled      = "cancelled"
	BuildAttemptTimedOut       = "timed_out"
	BuildAttemptSuperseded     = "superseded"
)

const buildRunSelectColumns = `id, service_id, commit_sha, commit_message, commit_contributors, state, COALESCE(builder_id, ''), owner_epoch, lease_expires_at,
		attempt_count, attempt_limit, cancel_requested_at, deadline_at, last_heartbeat_at,
		COALESCE(artifact_id, ''), COALESCE((SELECT image_ref FROM build_artifacts WHERE id = build_runs.artifact_id), ''), failure_reason,
	        COALESCE(source_revision_id, ''), COALESCE(source_snapshot_id, ''), source_snapshot_digest, build_actor_kind, build_actor_id, target_rollout_generation, build_recipe_json,
	        queued_at, started_at, finished_at`

func (s *persistence) latestBuildForServiceQuerier(ctx context.Context, q ServiceQueryer, buildID string) (*platformv1.BuildStatus, error) {
	if buildID == "" {
		return nil, nil
	}
	rec, err := s.buildRunByIDQuerier(ctx, q, buildID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return ToProtoBuildStatus(rec), nil
}

func (s *persistence) buildRunByIDQuerier(ctx context.Context, q ServiceQueryer, buildID string) (BuildRunRecord, error) {
	rec, err := scanBuildRunRow(q.QueryRowContext(ctx,
		`SELECT `+buildRunSelectColumns+`
		   FROM build_runs
		  WHERE id = $1`,
		buildID,
	))
	if err != nil {
		return BuildRunRecord{}, err
	}
	err = q.QueryRowContext(ctx, `SELECT e.project_id, s.environment_id FROM services s
		JOIN environments e ON e.id = s.environment_id WHERE s.id = $1`, rec.ServiceID).
		Scan(&rec.ProjectID, &rec.EnvironmentID)
	if err != nil {
		return BuildRunRecord{}, err
	}
	if rec.ArtifactID != "" {
		artifact, err := s.buildArtifactByIDQuerier(ctx, q, rec.ArtifactID)
		if err != nil {
			return BuildRunRecord{}, err
		}
		rec.Artifact = &artifact
	}
	return rec, nil
}

func sourceSnapshotMatchesRevision(snapshot source.SourceSnapshotRecord, revision source.SourceRevisionRecord) bool {
	if snapshot.SourceRevisionID == revision.ID {
		return true
	}
	return snapshot.Provider == revision.Provider &&
		snapshot.ProviderRepositoryExternalID == revision.ProviderRepositoryExternalID &&
		snapshot.CommitSHA == revision.CommitSHA
}

func BuildStateTerminal(state string) bool {
	switch state {
	case BuildStateSucceeded, BuildStateFailed, BuildStateSuperseded, BuildStateCancelled:
		return true
	default:
		return false
	}
}

func (s *persistence) serviceByIDInternalQuerier(ctx context.Context, q ServiceQueryer, serviceID string) (ServiceRecord, error) {
	row := q.QueryRowContext(ctx,
		serviceSelectSQL+` WHERE s.id = $1`,
		serviceID,
	)
	rec, err := scanServiceRow(row)
	if err != nil {
		return ServiceRecord{}, err
	}
	rec.Spec, err = s.loadServiceDetailsQuerier(ctx, q, rec.ID, rec.SpecRevision)
	if err != nil {
		return ServiceRecord{}, err
	}
	rec.SourceSummary, err = s.loadServiceSourceSummaryQuerier(ctx, q, rec.Spec, rec.ID)
	if err != nil {
		return ServiceRecord{}, err
	}
	rec.LatestBuild, err = s.latestBuildForServiceQuerier(ctx, q, rec.LatestBuildID)
	if err != nil {
		return ServiceRecord{}, err
	}
	if err := s.attachLatestDeploymentQuerier(ctx, q, &rec); err != nil {
		return ServiceRecord{}, err
	}
	return rec, nil
}
