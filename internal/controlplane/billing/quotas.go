package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/encoding/protojson"
)

// Quota validation runs inside the caller's product transaction on
// desired-state mutations, mirroring the ServiceEnforcedTx restriction
// helpers: pure functions of journal state, gated by the caller's
// enforce flag (enabled billing mode only). Workspaces without a billing
// account — managed platform projects, or billing not yet provisioned —
// are never quota-checked. Violations wrap ErrGrowthBlocked so the
// console's existing blocked-action display recognizes them.
//
// A mutation is refused only when it pushes a dimension over its limit:
// when the workspace is already over quota, any mutation that does not
// increase the exceeded dimension (scale-down, shrink toward the limit)
// stays allowed.

func quotaExceeded(format string, args ...any) error {
	return fmt.Errorf("%w: plan quota exceeded: %s", ErrGrowthBlocked, fmt.Sprintf(format, args...))
}

// checkQuotaDimension refuses a resulting total that exceeds its limit by
// growing past the current total.
func checkQuotaDimension(name string, current, resulting, limit int64, formatResulting, formatLimit string) error {
	if resulting > limit && resulting > current {
		return quotaExceeded("%s %s exceeds the %s limit", name, formatResulting, formatLimit)
	}
	return nil
}

// accountPlanTx resolves a workspace's billing account and plan. found is
// false for workspaces without an account or with an unknown plan.
func accountPlanTx(ctx context.Context, tx *sql.Tx, workspaceID string) (Account, Plan, bool, error) {
	account, err := accountByWorkspace(ctx, tx, workspaceID)
	if errors.Is(err, ErrNoAccount) {
		return Account{}, Plan{}, false, nil
	}
	if err != nil {
		return Account{}, Plan{}, false, err
	}
	plan, ok := Plans[account.PlanID]
	return account, plan, ok, nil
}

// serviceUsage is one live service's desired resource footprint: its
// draft spec's limits times its desired replicas.
type serviceUsage struct {
	serviceID     string
	environmentID string
	cpuMillis     int64
	memoryMebis   int64
	replicas      int64
}

// specUsage extracts the desired footprint of one service spec.
func specUsage(spec *platformv1.ServiceSpec, fallbackReplicas int64) serviceUsage {
	replicas := fallbackReplicas
	if spec != nil && spec.DesiredReplicaCount != nil {
		replicas = int64(spec.GetDesiredReplicaCount())
	}
	if replicas < 0 {
		replicas = 0
	}
	var usage serviceUsage
	usage.replicas = replicas
	if runtime := spec.GetRuntime(); runtime != nil {
		usage.cpuMillis = runtime.GetCpuMillis() * replicas
		usage.memoryMebis = runtime.GetMemoryMebibytes() * replicas
	}
	return usage
}

// workspaceServiceUsagesTx loads the desired footprint of every live
// service in a workspace from its current (draft) spec revision.
func workspaceServiceUsagesTx(ctx context.Context, tx *sql.Tx, workspaceID string) ([]serviceUsage, error) {
	rows, err := tx.QueryContext(ctx, `SELECT s.id, s.environment_id, s.desired_replica_count, r.spec_json
		  FROM services s
		  JOIN environments e ON e.id = s.environment_id
		  JOIN projects p ON p.id = e.project_id
		  JOIN service_revisions r ON r.service_id = s.id AND r.spec_revision = s.current_spec_revision
		 WHERE p.workspace_id = $1
		   AND s.deleted_at IS NULL AND e.deleted_at IS NULL AND p.deleted_at IS NULL`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []serviceUsage
	for rows.Next() {
		var serviceID, environmentID string
		var columnReplicas int64
		var rawSpec []byte
		if err := rows.Scan(&serviceID, &environmentID, &columnReplicas, &rawSpec); err != nil {
			return nil, err
		}
		spec := &platformv1.ServiceSpec{}
		if err := protojson.Unmarshal(rawSpec, spec); err != nil {
			return nil, fmt.Errorf("billing: parse service spec for quota: %w", err)
		}
		usage := specUsage(spec, columnReplicas)
		usage.serviceID, usage.environmentID = serviceID, environmentID
		out = append(out, usage)
	}
	return out, rows.Err()
}

// ValidateServiceQuotaTx refuses a service create (replaceServiceID empty)
// or update/scale whose resulting workspace totals cross the plan quota.
func ValidateServiceQuotaTx(ctx context.Context, tx *sql.Tx, enforce bool, environmentID, replaceServiceID string, spec *platformv1.ServiceSpec) error {
	if !enforce {
		return nil
	}
	var workspaceID sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT p.workspace_id FROM environments e
		JOIN projects p ON p.id = e.project_id WHERE e.id = $1`, environmentID).Scan(&workspaceID)
	if errors.Is(err, sql.ErrNoRows) || !workspaceID.Valid || workspaceID.String == "" {
		return err
	}
	if err != nil {
		return err
	}
	_, plan, found, err := accountPlanTx(ctx, tx, workspaceID.String)
	if err != nil || !found {
		return err
	}
	usages, err := workspaceServiceUsagesTx(ctx, tx, workspaceID.String)
	if err != nil {
		return err
	}
	var currentCPU, currentMem, envCount int64
	var replaced serviceUsage
	for _, usage := range usages {
		currentCPU += usage.cpuMillis
		currentMem += usage.memoryMebis
		if usage.environmentID == environmentID {
			envCount++
		}
		if usage.serviceID == replaceServiceID {
			replaced = usage
		}
	}
	candidate := specUsage(spec, replaced.replicas)
	create := replaceServiceID == ""
	resultingCPU := currentCPU - replaced.cpuMillis + candidate.cpuMillis
	resultingMem := currentMem - replaced.memoryMebis + candidate.memoryMebis
	resultingEnvCount := envCount
	if create {
		resultingEnvCount++
	}
	quota := plan.Quota
	if err := checkQuotaDimension("workspace CPU", currentCPU, resultingCPU, quota.MaxCPUMillis,
		fmt.Sprintf("%dm", resultingCPU), fmt.Sprintf("%dm", quota.MaxCPUMillis)); err != nil {
		return err
	}
	if err := checkQuotaDimension("workspace memory", currentMem, resultingMem, quota.MaxMemoryMebibytes,
		fmt.Sprintf("%d MiB", resultingMem), fmt.Sprintf("%d MiB", quota.MaxMemoryMebibytes)); err != nil {
		return err
	}
	if err := checkQuotaDimension("services in this environment", envCount, resultingEnvCount, quota.MaxServicesPerEnvironment,
		fmt.Sprintf("%d", resultingEnvCount), fmt.Sprintf("%d", quota.MaxServicesPerEnvironment)); err != nil {
		return err
	}
	return checkQuotaDimension("replicas for this service", replaced.replicas, candidate.replicas, quota.MaxReplicasPerService,
		fmt.Sprintf("%d", candidate.replicas), fmt.Sprintf("%d", quota.MaxReplicasPerService))
}

// ValidateProjectQuotaTx refuses a project creation that crosses the
// workspace's project limit.
func ValidateProjectQuotaTx(ctx context.Context, tx *sql.Tx, enforce bool, workspaceID string) error {
	if !enforce || workspaceID == "" {
		return nil
	}
	_, plan, found, err := accountPlanTx(ctx, tx, workspaceID)
	if err != nil || !found {
		return err
	}
	var current int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM projects
		WHERE workspace_id = $1 AND deleted_at IS NULL`, workspaceID).Scan(&current); err != nil {
		return err
	}
	return checkQuotaDimension("projects in this workspace", current, current+1, plan.Quota.MaxProjects,
		fmt.Sprintf("%d", current+1), fmt.Sprintf("%d", plan.Quota.MaxProjects))
}

// workspaceVolumeBytesTx sums provisioned volume sizes over a workspace's
// live volumes.
func workspaceVolumeBytesTx(ctx context.Context, tx *sql.Tx, workspaceID string) (int64, error) {
	var total int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(sum(v.size_bytes), 0)
		  FROM volumes v
		  JOIN environments e ON e.id = v.environment_id
		  JOIN projects p ON p.id = e.project_id
		 WHERE p.workspace_id = $1
		   AND v.deleted_at IS NULL AND e.deleted_at IS NULL AND p.deleted_at IS NULL`, workspaceID).Scan(&total)
	return total, err
}

func checkVolumeQuotaTx(ctx context.Context, tx *sql.Tx, workspaceID string, deltaBytes int64) error {
	_, plan, found, err := accountPlanTx(ctx, tx, workspaceID)
	if err != nil || !found {
		return err
	}
	current, err := workspaceVolumeBytesTx(ctx, tx, workspaceID)
	if err != nil {
		return err
	}
	return checkQuotaDimension("workspace volume storage", current, current+deltaBytes, plan.Quota.MaxVolumeBytes,
		fmt.Sprintf("%d bytes", current+deltaBytes), fmt.Sprintf("%d bytes", plan.Quota.MaxVolumeBytes))
}

// ValidateVolumeCreateTx refuses a volume creation of sizeBytes that
// crosses the workspace's provisioned-volume limit.
func ValidateVolumeCreateTx(ctx context.Context, tx *sql.Tx, enforce bool, workspaceID string, sizeBytes int64) error {
	if !enforce || workspaceID == "" {
		return nil
	}
	return checkVolumeQuotaTx(ctx, tx, workspaceID, sizeBytes)
}

// ValidateVolumeGrowTx refuses growing a volume to newSizeBytes when the
// growth crosses the workspace's provisioned-volume limit.
func ValidateVolumeGrowTx(ctx context.Context, tx *sql.Tx, enforce bool, volumeID string, newSizeBytes int64) error {
	if !enforce {
		return nil
	}
	var workspaceID sql.NullString
	var currentSize int64
	err := tx.QueryRowContext(ctx, `SELECT p.workspace_id, v.size_bytes
		  FROM volumes v
		  JOIN environments e ON e.id = v.environment_id
		  JOIN projects p ON p.id = e.project_id
		 WHERE v.id = $1`, volumeID).Scan(&workspaceID, &currentSize)
	if errors.Is(err, sql.ErrNoRows) || !workspaceID.Valid || workspaceID.String == "" {
		return err
	}
	if err != nil {
		return err
	}
	return checkVolumeQuotaTx(ctx, tx, workspaceID.String, newSizeBytes-currentSize)
}

// BuildClaimBlockedTx reports whether the plan's build caps (concurrent
// builds per workspace, build-minutes per rolling window) refuse a new
// build claim for a service. The build scheduler calls it next to
// ServiceGrowthBlockedTx; a blocked build stays queued.
// Build run states are delivery's literals; 'running' is BuildStateRunning.
func BuildClaimBlockedTx(ctx context.Context, tx *sql.Tx, serviceID string, now time.Time) (bool, string, error) {
	var workspaceID sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT p.workspace_id
		  FROM services s
		  JOIN environments e ON e.id = s.environment_id
		  JOIN projects p ON p.id = e.project_id
		 WHERE s.id = $1`, serviceID).Scan(&workspaceID)
	if errors.Is(err, sql.ErrNoRows) || !workspaceID.Valid || workspaceID.String == "" {
		return false, "", err
	}
	if err != nil {
		return false, "", err
	}
	_, plan, found, err := accountPlanTx(ctx, tx, workspaceID.String)
	if err != nil || !found {
		return false, "", err
	}
	quota := plan.Quota
	if quota.MaxConcurrentBuilds > 0 {
		var running int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*)
			  FROM build_runs b
			  JOIN services s ON s.id = b.service_id
			  JOIN environments e ON e.id = s.environment_id
			  JOIN projects p ON p.id = e.project_id
			 WHERE p.workspace_id = $1 AND b.state = 'running'`, workspaceID.String).Scan(&running); err != nil {
			return false, "", err
		}
		if running >= quota.MaxConcurrentBuilds {
			return true, "build concurrency limit reached for this workspace", nil
		}
	}
	if quota.MaxBuildMinutes30Days > 0 {
		var seconds float64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(sum(EXTRACT(EPOCH FROM COALESCE(b.finished_at, $2) - b.started_at)), 0)
			  FROM build_runs b
			  JOIN services s ON s.id = b.service_id
			  JOIN environments e ON e.id = s.environment_id
			  JOIN projects p ON p.id = e.project_id
			 WHERE p.workspace_id = $1 AND b.started_at IS NOT NULL AND b.started_at >= $3`,
			workspaceID.String, now, now.Add(-BuildMinutesWindow)).Scan(&seconds); err != nil {
			return false, "", err
		}
		if seconds >= float64(quota.MaxBuildMinutes30Days)*60 {
			return true, "build minutes limit reached for this workspace", nil
		}
	}
	return false, "", nil
}

// CheckCustomDomainAllowed refuses custom domain bindings on the trial
// plan until the account has a saved payment method. Paid plans and
// workspaces without a billing account are unaffected.
func CheckCustomDomainAllowed(ctx context.Context, db *sql.DB, serviceID string) error {
	var workspaceID sql.NullString
	err := db.QueryRowContext(ctx, `SELECT p.workspace_id
		  FROM services s
		  JOIN environments e ON e.id = s.environment_id
		  JOIN projects p ON p.id = e.project_id
		 WHERE s.id = $1`, serviceID).Scan(&workspaceID)
	if errors.Is(err, sql.ErrNoRows) || !workspaceID.Valid || workspaceID.String == "" {
		return err
	}
	if err != nil {
		return err
	}
	account, err := accountByWorkspace(ctx, db, workspaceID.String)
	if errors.Is(err, ErrNoAccount) {
		return nil
	}
	if err != nil {
		return err
	}
	if account.PlanID == PlanTrial && !account.PaymentReady {
		return fmt.Errorf("%w: custom domains require a saved payment method on the trial plan", ErrGrowthBlocked)
	}
	return nil
}
