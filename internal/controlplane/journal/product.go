package journal

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// readProductState reads only durable product tables, in the transaction that
// changed them. Runtime presence and health must never enter a durable command.
func readProductState(ctx context.Context, tx *sql.Tx) (DurableState, error) {
	state := DurableState{}
	rows, err := tx.QueryContext(ctx, `
SELECT 'Projects', id, jsonb_build_object('id', id, 'name', name, 'kind', kind, 'system_key', system_key, 'owner_user_id', owner_user_id, 'created_at', created_at) FROM projects
UNION ALL
SELECT 'Services', id::STRING, jsonb_build_object('id', id, 'environment_id', environment_id, 'name', name, 'current_spec_revision', current_spec_revision, 'current_rollout_generation', current_rollout_generation, 'current_resolved_image', current_resolved_image, 'last_successful_commit_sha', last_successful_commit_sha, 'latest_build_id', latest_build_id, 'desired_replica_count', desired_replica_count, 'placement_message', placement_message, 'created_at', created_at, 'updated_at', updated_at) FROM services
UNION ALL
SELECT 'Revisions', service_id::STRING || '/' || spec_revision::STRING, jsonb_build_object('service_id', service_id, 'spec_revision', spec_revision, 'spec_json', spec_json, 'created_at', created_at) FROM service_revisions
UNION ALL
SELECT 'Assignments', id::STRING, jsonb_build_object('id', id, 'service_id', service_id, 'deployment_id', deployment_id, 'agent_id', agent_id, 'desired_spec_revision', desired_spec_revision, 'desired_rollout_generation', desired_rollout_generation, 'allocation_ipv4', allocation_ipv4, 'allocation_ipv6', allocation_ipv6, 'operator_restart_nonce', operator_restart_nonce, 'rollout_state', rollout_state, 'intent', intent, 'intent_message', intent_message, 'drain_started_at', drain_started_at, 'drain_deadline', drain_deadline, 'created_at', created_at, 'updated_at', updated_at) FROM allocation_assignments
UNION ALL
SELECT 'Rollouts', service_id::STRING || '/' || rollout_generation::STRING, jsonb_build_object('service_id', service_id, 'rollout_generation', rollout_generation, 'spec_revision', spec_revision, 'reason', reason, 'build_id', build_id, 'requested_by_user_id', requested_by_user_id, 'state', state, 'strategy_json', strategy_json, 'desired_replica_count', desired_replica_count, 'image_digest', image_digest, 'failure_reason', failure_reason, 'target_allocation_id', target_allocation_id, 'completed_at', completed_at, 'progress_at', progress_at, 'created_at', created_at) FROM service_rollouts
UNION ALL
SELECT 'Deployments', id::STRING, jsonb_build_object('id', id, 'service_id', service_id, 'spec_revision', spec_revision, 'rollout_generation', rollout_generation, 'build_id', build_id, 'image_digest', image_digest, 'state', state, 'cause_kind', cause_kind, 'cause_id', cause_id, 'reason_code', reason_code, 'detail', detail, 'resolved_spec_json', resolved_spec_json, 'variable_versions_json', variable_versions_json, 'is_current', is_current, 'requested_by_user_id', requested_by_user_id, 'created_at', created_at, 'updated_at', updated_at) FROM deployments
UNION ALL
SELECT 'Agents', id::STRING, jsonb_build_object('id', id, 'name', name, 'local_store_id', local_store_id, 'session_incarnation', session_incarnation, 'region', region, 'zone', zone, 'failure_domain', failure_domain, 'reserved_cpu_millis', reserved_cpu_millis, 'reserved_memory_mebibytes', reserved_memory_mebibytes, 'advertise_addr', advertise_addr, 'workload_ipv4_subnet', workload_ipv4_subnet, 'workload_ipv6_subnet', workload_ipv6_subnet, 'wireguard_public_key', wireguard_public_key, 'wireguard_listen_port', wireguard_listen_port, 'wireguard_ipv6', wireguard_ipv6, 'cpu_millis_capacity', cpu_millis_capacity, 'memory_mebibytes_capacity', memory_mebibytes_capacity, 'runtime_capabilities', runtime_capabilities, 'software_version', software_version, 'created_at', created_at, 'updated_at', updated_at, 'desired_revision', desired_revision) FROM agent_registrations
UNION ALL
SELECT 'Administration', agent_id::STRING, jsonb_build_object('agent_id', agent_id, 'lifecycle_state', lifecycle_state, 'operator_intent', operator_intent, 'maintenance_message', maintenance_message, 'credential_revoked_at', credential_revoked_at, 'updated_at', updated_at) FROM agent_administration
UNION ALL
SELECT 'Environments', id::STRING, jsonb_build_object('id', id, 'project_id', project_id, 'name', name, 'kind', kind, 'is_production', is_production, 'network_identity', network_identity, 'copied_from_environment_id', copied_from_environment_id, 'created_at', created_at, 'updated_at', updated_at) FROM environments
UNION ALL
SELECT 'Volumes', id::STRING, jsonb_build_object('id', id, 'environment_id', environment_id, 'name', name, 'size_bytes', size_bytes, 'created_at', created_at) FROM volumes
UNION ALL
SELECT 'Domains', hostname::STRING, jsonb_build_object('hostname', hostname, 'service_id', service_id, 'target_port', target_port, 'platform_generated', platform_generated, 'created_at', created_at, 'updated_at', updated_at) FROM domain_bindings`)
	if err != nil {
		return state, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, key string
		var raw []byte
		if err := rows.Scan(&kind, &key, &raw); err != nil {
			return state, err
		}
		switch kind {
		case "Projects":
			if err := decodeRecord(&state.Projects, key, raw); err != nil {
				return state, fmt.Errorf("decode projects: %w", err)
			}
		case "Services":
			if err := decodeRecord(&state.Services, key, raw); err != nil {
				return state, fmt.Errorf("decode services: %w", err)
			}
		case "Revisions":
			if err := decodeRecord(&state.Revisions, key, raw); err != nil {
				return state, fmt.Errorf("decode service_revisions: %w", err)
			}
		case "Assignments":
			if err := decodeRecord(&state.Assignments, key, raw); err != nil {
				return state, fmt.Errorf("decode allocation_assignments: %w", err)
			}
		case "Rollouts":
			if err := decodeRecord(&state.Rollouts, key, raw); err != nil {
				return state, fmt.Errorf("decode service_rollouts: %w", err)
			}
		case "Deployments":
			if err := decodeRecord(&state.Deployments, key, raw); err != nil {
				return state, fmt.Errorf("decode deployments: %w", err)
			}
		case "Agents":
			if err := decodeRecord(&state.Agents, key, raw); err != nil {
				return state, fmt.Errorf("decode agent_registrations: %w", err)
			}
		case "Administration":
			if err := decodeRecord(&state.Administration, key, raw); err != nil {
				return state, fmt.Errorf("decode agent_administration: %w", err)
			}
		case "Environments":
			if err := decodeRecord(&state.Environments, key, raw); err != nil {
				return state, fmt.Errorf("decode environments: %w", err)
			}
		case "Volumes":
			if err := decodeRecord(&state.Volumes, key, raw); err != nil {
				return state, fmt.Errorf("decode volumes: %w", err)
			}
		case "Domains":
			if err := decodeRecord(&state.Domains, key, raw); err != nil {
				return state, fmt.Errorf("decode domain_bindings: %w", err)
			}
		}
	}
	return state, rows.Err()
}

func decodeRecord[T any](records *map[string]T, key string, raw []byte) error {
	var value T
	// JSONB renders whitespace differently from encoding/json. Normalize nested
	// RawMessage fields too, so replay and a fresh product read compare equally.
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return err
	}
	if err := json.Unmarshal(compact.Bytes(), &value); err != nil {
		return err
	}
	if *records == nil {
		*records = make(map[string]T)
	}
	(*records)[key] = value
	return nil
}
