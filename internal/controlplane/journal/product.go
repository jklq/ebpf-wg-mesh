package journal

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

type productTable struct {
	name    string
	keySQL  string
	jsonSQL string
	// filter excludes tombstoned rows (and rows under a tombstoned
	// ancestor) from durable state. Tombstoning therefore reads as removal
	// to every journal consumer — agents, ingress, rollout reconciliation —
	// while the rows stay recoverable until garbage collection.
	filter string
	decode func(*DurableState, string, []byte) error
}

func readProductState(ctx context.Context, tx *sql.Tx) (DurableState, error) {
	// Keep empty tables canonical across replay and normalized-table rebuilds.
	// Applying deletions leaves non-nil empty maps, so a rebuild must do the
	// same or replicas can disagree despite containing identical rows.
	state := DurableState{
		Projects:       make(map[string]Project),
		Services:       make(map[string]ServiceIntent),
		Revisions:      make(map[string]ServiceRevision),
		Assignments:    RowMap[Assignment]{},
		Rollouts:       make(map[string]Rollout),
		Deployments:    make(map[string]Deployment),
		Agents:         make(map[string]AgentRegistration),
		Administration: make(map[string]AgentAdministration),
		Environments:   make(map[string]Environment),
		Volumes:        make(map[string]Volume),
		Destructions:   make(map[string]VolumeDestruction),
		Domains:        make(map[string]Domain),
	}
	for _, table := range productTables {
		query := `SELECT ` + table.keySQL + `, ` + table.jsonSQL + ` FROM ` + table.name
		if table.filter != "" {
			query += ` WHERE ` + table.filter
		}
		rows, err := tx.QueryContext(ctx, query)
		if err != nil {
			return state, err
		}
		if err := scanProductRows(rows, &state, table); err != nil {
			rows.Close()
			return state, err
		}
		if err := rows.Close(); err != nil {
			return state, err
		}
	}
	return state, nil
}

func scanProductRows(rows *sql.Rows, state *DurableState, table productTable) error {
	for rows.Next() {
		var key string
		var raw []byte
		if err := rows.Scan(&key, &raw); err != nil {
			return err
		}
		if err := table.decode(state, key, raw); err != nil {
			return fmt.Errorf("decode %s: %w", table.name, err)
		}
	}
	return rows.Err()
}

const projectJSON = `jsonb_build_object('id', id, 'name', name, 'kind', kind, 'system_key', system_key, 'created_at', created_at)`
const serviceJSON = `jsonb_build_object(
	'id', id,
	'environment_id', environment_id,
	'name', name,
	'current_spec_revision', current_spec_revision,
	'current_rollout_generation', COALESCE((SELECT current_rollout_generation FROM service_delivery_status WHERE service_id = services.id), 0),
	'current_artifact_id', COALESCE((SELECT current_artifact_id FROM service_delivery_status WHERE service_id = services.id), ''),
	'current_resolved_image', COALESCE((SELECT a.image_ref FROM build_artifacts a JOIN service_delivery_status ds ON ds.current_artifact_id = a.id WHERE ds.service_id = services.id), ''),
	'last_successful_commit_sha', COALESCE((SELECT last_successful_commit_sha FROM service_delivery_status WHERE service_id = services.id), ''),
	'latest_build_id', COALESCE((SELECT latest_build_id FROM service_delivery_status WHERE service_id = services.id), ''),
	'desired_replica_count', desired_replica_count,
	'placement_message', COALESCE((SELECT placement_message FROM service_delivery_status WHERE service_id = services.id), ''),
	'created_at', created_at,
	'updated_at', GREATEST(updated_at, (SELECT updated_at FROM service_delivery_status WHERE service_id = services.id))
)`
const revisionJSON = `jsonb_build_object('service_id', service_id, 'spec_revision', spec_revision, 'spec_json', spec_json, 'env_dek_id', COALESCE(env_dek_id, ''), 'env_ciphertext', COALESCE(encode(env_ciphertext, 'base64'), ''), 'created_at', created_at)`
const assignmentJSON = `jsonb_build_object('id', id, 'service_id', service_id, 'deployment_id', deployment_id, 'agent_id', agent_id, 'desired_spec_revision', desired_spec_revision, 'desired_rollout_generation', desired_rollout_generation, 'allocation_ipv4', allocation_ipv4, 'allocation_ipv6', allocation_ipv6, 'operator_restart_nonce', operator_restart_nonce, 'rollout_state', rollout_state, 'intent', intent, 'intent_message', intent_message, 'drain_started_at', drain_started_at, 'drain_deadline', drain_deadline, 'created_at', created_at, 'updated_at', updated_at)`
const rolloutJSON = `jsonb_build_object('service_id', service_id, 'rollout_generation', rollout_generation, 'spec_revision', spec_revision, 'reason', reason, 'build_id', build_id, 'requested_by_user_id', requested_by_user_id, 'state', state, 'strategy_json', strategy_json, 'desired_replica_count', desired_replica_count, 'artifact_id', COALESCE(artifact_id, ''), 'image_digest', COALESCE((SELECT image_ref FROM build_artifacts WHERE id = service_rollouts.artifact_id), ''), 'failure_reason', failure_reason, 'target_allocation_id', target_allocation_id, 'recreate', recreate, 'completed_at', completed_at, 'progress_at', progress_at, 'created_at', created_at)`
const deploymentJSON = `jsonb_build_object('id', id, 'service_id', service_id, 'spec_revision', spec_revision, 'rollout_generation', rollout_generation, 'source_revision_id', COALESCE(source_revision_id, ''), 'build_id', build_id, 'artifact_id', COALESCE(artifact_id, ''), 'image_digest', COALESCE((SELECT image_ref FROM build_artifacts WHERE id = deployments.artifact_id), ''), 'state', state, 'cause_kind', cause_kind, 'cause_id', cause_id, 'reason_code', reason_code, 'detail', detail, 'is_current', is_current, 'requested_by_user_id', requested_by_user_id, 'created_at', created_at, 'updated_at', updated_at)`
const agentJSON = `jsonb_build_object('id', id, 'name', name, 'local_store_id', local_store_id, 'session_incarnation', session_incarnation, 'region', region, 'zone', zone, 'failure_domain', failure_domain, 'reserved_cpu_millis', reserved_cpu_millis, 'reserved_memory_mebibytes', reserved_memory_mebibytes, 'advertise_addr', advertise_addr, 'workload_ipv4_subnet', workload_ipv4_subnet, 'workload_ipv6_subnet', workload_ipv6_subnet, 'wireguard_public_key', wireguard_public_key, 'wireguard_listen_port', wireguard_listen_port, 'wireguard_endpoint', wireguard_endpoint, 'wireguard_ipv6', wireguard_ipv6, 'cpu_millis_capacity', cpu_millis_capacity, 'memory_mebibytes_capacity', memory_mebibytes_capacity, 'runtime_capabilities', runtime_capabilities, 'software_version', software_version, 'created_at', created_at, 'updated_at', updated_at, 'desired_revision', desired_revision)`
const administrationJSON = `jsonb_build_object('host_type', host_type, 'agent_id', agent_id, 'lifecycle_state', lifecycle_state, 'operator_intent', operator_intent, 'maintenance_message', maintenance_message, 'credential_revoked_at', credential_revoked_at, 'updated_at', updated_at)`
const environmentJSON = `jsonb_build_object('id', id, 'project_id', project_id, 'name', name, 'kind', kind, 'is_production', is_production, 'auto_deploy', auto_deploy, 'network_identity', network_identity, 'copied_from_environment_id', copied_from_environment_id, 'created_at', created_at, 'updated_at', updated_at)`
const volumeJSON = `jsonb_build_object('id', id, 'environment_id', environment_id, 'name', name, 'size_bytes', size_bytes, 'agent_id', COALESCE(agent_id, ''), 'created_at', created_at)`
const destructionJSON = `jsonb_build_object('volume_id', volume_id, 'agent_id', agent_id, 'requested_at', requested_at)`
const domainJSON = `jsonb_build_object('hostname', hostname, 'service_id', service_id, 'target_port', target_port, 'platform_generated', platform_generated, 'created_at', created_at, 'updated_at', updated_at)`

// Tombstone filters hide soft-deleted rows from durable state. A tombstoned
// resource reads as removed to agents, ingress, and reconcilers; restoring it
// re-adds the rows. Each predicate must be valid both as a standalone WHERE
// clause and conjoined with a key predicate.
const projectLiveFilter = `projects.deleted_at IS NULL`

const environmentLiveFilter = `environments.deleted_at IS NULL
		AND NOT EXISTS (SELECT 1 FROM projects p
			WHERE p.id = environments.project_id AND p.deleted_at IS NOT NULL)`

const serviceLiveFilter = `services.deleted_at IS NULL
		AND NOT EXISTS (SELECT 1 FROM environments e JOIN projects p ON p.id = e.project_id
			WHERE e.id = services.environment_id
			  AND (e.deleted_at IS NOT NULL OR p.deleted_at IS NOT NULL))`

func serviceChildLiveFilter(table, column string) string {
	return `NOT EXISTS (SELECT 1 FROM services s
			JOIN environments e ON e.id = s.environment_id
			JOIN projects p ON p.id = e.project_id
			WHERE s.id = ` + table + `.` + column + `
			  AND (s.deleted_at IS NOT NULL OR e.deleted_at IS NOT NULL OR p.deleted_at IS NOT NULL))`
}

// History remains in PostgreSQL. The projection keeps current intent, unfinished
// work and the rows referenced by allocations, including draining generations.
const deploymentOperationalFilter = `(is_current OR state IN
	('staged', 'queued_build', 'building', 'scheduling', 'image_pull', 'starting', 'readiness', 'active', 'draining')
	OR EXISTS (SELECT 1 FROM allocation_assignments a WHERE a.deployment_id = deployments.id))`

const rolloutOperationalFilter = `(state IN ('pending_build', 'in_progress')
	OR EXISTS (SELECT 1 FROM service_delivery_status d WHERE d.service_id = service_rollouts.service_id
		AND d.current_rollout_generation = service_rollouts.rollout_generation)
	OR EXISTS (SELECT 1 FROM allocation_assignments a WHERE a.service_id = service_rollouts.service_id
		AND a.desired_rollout_generation = service_rollouts.rollout_generation))`

const revisionOperationalFilter = `(EXISTS (SELECT 1 FROM services s WHERE s.id = service_revisions.service_id
		AND s.current_spec_revision = service_revisions.spec_revision)
	OR EXISTS (SELECT 1 FROM allocation_assignments a WHERE a.service_id = service_revisions.service_id
		AND a.desired_spec_revision = service_revisions.spec_revision)
	OR EXISTS (SELECT 1 FROM service_rollouts WHERE service_id = service_revisions.service_id
		AND spec_revision = service_revisions.spec_revision AND ` + rolloutOperationalFilter + `)
	OR EXISTS (SELECT 1 FROM deployments WHERE service_id = service_revisions.service_id
		AND spec_revision = service_revisions.spec_revision AND ` + deploymentOperationalFilter + `))`

func operationalFilter(table string) string {
	filter := serviceChildLiveFilter(table, "service_id")
	switch table {
	case "service_revisions":
		return filter + ` AND ` + revisionOperationalFilter
	case "service_rollouts":
		return filter + ` AND ` + rolloutOperationalFilter
	case "deployments":
		return filter + ` AND ` + deploymentOperationalFilter
	default:
		panic("unknown operational table: " + table)
	}
}

const volumeLiveFilter = `volumes.deleted_at IS NULL
		AND NOT EXISTS (SELECT 1 FROM environments e JOIN projects p ON p.id = e.project_id
			WHERE e.id = volumes.environment_id
			  AND (e.deleted_at IS NOT NULL OR p.deleted_at IS NOT NULL))`

const domainLiveFilter = `domain_bindings.deleted_at IS NULL
		AND NOT EXISTS (SELECT 1 FROM services s
			JOIN environments e ON e.id = s.environment_id
			JOIN projects p ON p.id = e.project_id
			WHERE s.id = domain_bindings.service_id
			  AND (s.deleted_at IS NOT NULL OR e.deleted_at IS NOT NULL OR p.deleted_at IS NOT NULL))`

var productTables = []productTable{
	{
		name: "projects", keySQL: "id", jsonSQL: projectJSON, filter: projectLiveFilter,
		decode: func(s *DurableState, key string, raw []byte) error { return decodeRecord(&s.Projects, key, raw) },
	},
	{
		name: "services", keySQL: "id::TEXT", jsonSQL: serviceJSON, filter: serviceLiveFilter,
		decode: func(s *DurableState, key string, raw []byte) error { return decodeRecord(&s.Services, key, raw) },
	},
	{
		name: "service_revisions", keySQL: "service_id::TEXT || '/' || spec_revision::TEXT", jsonSQL: revisionJSON, filter: operationalFilter("service_revisions"),
		decode: func(s *DurableState, key string, raw []byte) error { return decodeRecord(&s.Revisions, key, raw) },
	},
	{
		name: "allocation_assignments", keySQL: "id::TEXT", jsonSQL: assignmentJSON, filter: serviceChildLiveFilter("allocation_assignments", "service_id"),
		decode: func(s *DurableState, key string, raw []byte) error {
			value, err := decodeValue[Assignment](raw)
			if err == nil {
				s.Assignments.Set(key, value)
			}
			return err
		},
	},
	{
		name: "service_rollouts", keySQL: "service_id::TEXT || '/' || rollout_generation::TEXT", jsonSQL: rolloutJSON, filter: operationalFilter("service_rollouts"),
		decode: func(s *DurableState, key string, raw []byte) error { return decodeRecord(&s.Rollouts, key, raw) },
	},
	{
		name: "deployments", keySQL: "id::TEXT", jsonSQL: deploymentJSON, filter: operationalFilter("deployments"),
		decode: func(s *DurableState, key string, raw []byte) error { return decodeRecord(&s.Deployments, key, raw) },
	},
	{
		name: "agent_registrations", keySQL: "id::TEXT", jsonSQL: agentJSON,
		decode: func(s *DurableState, key string, raw []byte) error { return decodeRecord(&s.Agents, key, raw) },
	},
	{
		name: "agent_administration", keySQL: "agent_id::TEXT", jsonSQL: administrationJSON,
		decode: func(s *DurableState, key string, raw []byte) error { return decodeRecord(&s.Administration, key, raw) },
	},
	{
		name: "environments", keySQL: "id::TEXT", jsonSQL: environmentJSON, filter: environmentLiveFilter,
		decode: func(s *DurableState, key string, raw []byte) error { return decodeRecord(&s.Environments, key, raw) },
	},
	{
		name: "volumes", keySQL: "id::TEXT", jsonSQL: volumeJSON, filter: volumeLiveFilter,
		decode: func(s *DurableState, key string, raw []byte) error { return decodeRecord(&s.Volumes, key, raw) },
	},
	{
		name: "volume_destructions", keySQL: "volume_id::TEXT", jsonSQL: destructionJSON,
		decode: func(s *DurableState, key string, raw []byte) error { return decodeRecord(&s.Destructions, key, raw) },
	},
	{
		name: "domain_bindings", keySQL: "hostname::TEXT", jsonSQL: domainJSON, filter: domainLiveFilter,
		decode: func(s *DurableState, key string, raw []byte) error { return decodeRecord(&s.Domains, key, raw) },
	},
}

func decodeRecord[T any](records *map[string]T, key string, raw []byte) error {
	value, err := decodeValue[T](raw)
	if err != nil {
		return err
	}
	if *records == nil {
		*records = make(map[string]T)
	}
	(*records)[key] = value
	return nil
}

func decodeValue[T any](raw []byte) (T, error) {
	var value T
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return value, err
	}
	if err := json.Unmarshal(compact.Bytes(), &value); err != nil {
		return value, err
	}
	// PostgreSQL renders UTC SQL timestamps with +00:00. Go can parse that
	// into time.Local on a UTC host, while replayed JSON uses time.UTC (Z).
	// Canonicalize through the same encoding used by journal replay so structural
	// equality does not turn unchanged rows into phantom mutations.
	canonical, err := json.Marshal(value)
	if err != nil {
		return value, err
	}
	var normalized T
	if err := json.Unmarshal(canonical, &normalized); err != nil {
		return value, err
	}
	return normalized, nil
}
