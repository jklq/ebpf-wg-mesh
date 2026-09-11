package journal

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

type productTable struct {
	name    string
	keySQL  string
	jsonSQL string
	decode  func(*DurableState, string, []byte) error
	resolve func(context.Context, *sql.Tx, DurableState, []string) (Batch, error)
}

func readProductState(ctx context.Context, tx *sql.Tx) (DurableState, error) {
	state := DurableState{}
	for _, table := range productTables {
		rows, err := tx.QueryContext(ctx, `SELECT `+table.keySQL+`, `+table.jsonSQL+` FROM `+table.name)
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

func resolveRecorded(ctx context.Context, tx *sql.Tx, base DurableState, recorded map[Table]map[string]struct{}) (Batch, error) {
	batch := Batch{BaseIndex: base.LogIndex}
	for _, table := range productTables {
		keys := sortedKeys(recorded[table.id()])
		if len(keys) == 0 {
			continue
		}
		partial, err := table.resolve(ctx, tx, base, keys)
		if err != nil {
			return Batch{}, err
		}
		batch = mergeBatch(batch, partial)
	}
	return batch, nil
}

func (t productTable) id() Table { return Table(t.name) }

func mergeBatch(into, from Batch) Batch {
	into.Projects = append(into.Projects, from.Projects...)
	into.Services = append(into.Services, from.Services...)
	into.Revisions = append(into.Revisions, from.Revisions...)
	into.Assignments = append(into.Assignments, from.Assignments...)
	into.Rollouts = append(into.Rollouts, from.Rollouts...)
	into.Deployments = append(into.Deployments, from.Deployments...)
	into.Agents = append(into.Agents, from.Agents...)
	into.Administration = append(into.Administration, from.Administration...)
	into.Environments = append(into.Environments, from.Environments...)
	into.Volumes = append(into.Volumes, from.Volumes...)
	into.Domains = append(into.Domains, from.Domains...)
	return into
}

func sortedKeys(keys map[string]struct{}) []string {
	if len(keys) == 0 {
		return nil
	}
	out := make([]string, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	slices.Sort(out)
	return out
}

type keyedRead[T any] struct {
	keySQL  string
	jsonSQL string
	name    string
	decode  func([]byte) (T, error)
	where   func([]string) (string, []any)
}

func (t keyedRead[T]) read(ctx context.Context, tx *sql.Tx, keys []string) (map[string]T, error) {
	predicate, args := t.where(keys)
	rows, err := tx.QueryContext(ctx, `SELECT `+t.keySQL+`, `+t.jsonSQL+` FROM `+t.name+` WHERE `+predicate, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]T, len(keys))
	for rows.Next() {
		var key string
		var raw []byte
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, err
		}
		value, err := t.decode(raw)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", t.name, err)
		}
		out[key] = value
	}
	return out, rows.Err()
}

func resolveChanges[T any](ctx context.Context, tx *sql.Tx, base map[string]T, keys []string, table keyedRead[T]) ([]Change[T], error) {
	got, err := table.read(ctx, tx, keys)
	if err != nil {
		return nil, err
	}
	var result []Change[T]
	for _, key := range keys {
		value, ok := got[key]
		if !ok {
			if _, existed := base[key]; existed {
				result = append(result, Change[T]{Key: key})
			}
			continue
		}
		if previous, existed := base[key]; existed && reflect.DeepEqual(previous, value) {
			continue
		}
		result = append(result, Change[T]{Key: key, Value: &value})
	}
	slices.SortFunc(result, func(a, b Change[T]) int { return strings.Compare(a.Key, b.Key) })
	return result, nil
}

func singleKeyWhere(column string, keys []string) (string, []any) {
	placeholders := make([]string, len(keys))
	args := make([]any, len(keys))
	for i, key := range keys {
		placeholders[i] = "$" + strconv.Itoa(i+1)
		args[i] = key
	}
	return column + " IN (" + strings.Join(placeholders, ", ") + ")", args
}

func pairKeyWhere(left, right string, keys []string) (string, []any) {
	clauses := make([]string, 0, len(keys))
	args := make([]any, 0, 2*len(keys))
	for _, key := range keys {
		leftValue, rightValue, _ := strings.Cut(key, "/")
		clauses = append(clauses, fmt.Sprintf("(%s = $%d AND %s = $%d)", left, len(args)+1, right, len(args)+2))
		args = append(args, leftValue, rightValue)
	}
	return strings.Join(clauses, " OR "), args
}

func compositeKey(left string, right int64) string {
	return left + "/" + strconv.FormatInt(right, 10)
}

const projectJSON = `jsonb_build_object('id', id, 'name', name, 'kind', kind, 'system_key', system_key, 'owner_user_id', owner_user_id, 'created_at', created_at)`
const serviceJSON = `jsonb_build_object('id', id, 'environment_id', environment_id, 'name', name, 'current_spec_revision', current_spec_revision, 'current_rollout_generation', current_rollout_generation, 'current_resolved_image', current_resolved_image, 'last_successful_commit_sha', last_successful_commit_sha, 'latest_build_id', latest_build_id, 'desired_replica_count', desired_replica_count, 'placement_message', placement_message, 'created_at', created_at, 'updated_at', updated_at)`
const revisionJSON = `jsonb_build_object('service_id', service_id, 'spec_revision', spec_revision, 'spec_json', spec_json, 'created_at', created_at)`
const assignmentJSON = `jsonb_build_object('id', id, 'service_id', service_id, 'deployment_id', deployment_id, 'agent_id', agent_id, 'desired_spec_revision', desired_spec_revision, 'desired_rollout_generation', desired_rollout_generation, 'allocation_ipv4', allocation_ipv4, 'allocation_ipv6', allocation_ipv6, 'operator_restart_nonce', operator_restart_nonce, 'rollout_state', rollout_state, 'intent', intent, 'intent_message', intent_message, 'drain_started_at', drain_started_at, 'drain_deadline', drain_deadline, 'created_at', created_at, 'updated_at', updated_at)`
const rolloutJSON = `jsonb_build_object('service_id', service_id, 'rollout_generation', rollout_generation, 'spec_revision', spec_revision, 'reason', reason, 'build_id', build_id, 'requested_by_user_id', requested_by_user_id, 'state', state, 'strategy_json', strategy_json, 'desired_replica_count', desired_replica_count, 'image_digest', image_digest, 'failure_reason', failure_reason, 'target_allocation_id', target_allocation_id, 'completed_at', completed_at, 'progress_at', progress_at, 'created_at', created_at)`
const deploymentJSON = `jsonb_build_object('id', id, 'service_id', service_id, 'spec_revision', spec_revision, 'rollout_generation', rollout_generation, 'build_id', build_id, 'image_digest', image_digest, 'state', state, 'cause_kind', cause_kind, 'cause_id', cause_id, 'reason_code', reason_code, 'detail', detail, 'resolved_spec_json', resolved_spec_json, 'variable_versions_json', variable_versions_json, 'is_current', is_current, 'requested_by_user_id', requested_by_user_id, 'created_at', created_at, 'updated_at', updated_at)`
const agentJSON = `jsonb_build_object('id', id, 'name', name, 'local_store_id', local_store_id, 'session_incarnation', session_incarnation, 'region', region, 'zone', zone, 'failure_domain', failure_domain, 'reserved_cpu_millis', reserved_cpu_millis, 'reserved_memory_mebibytes', reserved_memory_mebibytes, 'advertise_addr', advertise_addr, 'workload_ipv4_subnet', workload_ipv4_subnet, 'workload_ipv6_subnet', workload_ipv6_subnet, 'wireguard_public_key', wireguard_public_key, 'wireguard_listen_port', wireguard_listen_port, 'wireguard_endpoint', wireguard_endpoint, 'wireguard_ipv6', wireguard_ipv6, 'cpu_millis_capacity', cpu_millis_capacity, 'memory_mebibytes_capacity', memory_mebibytes_capacity, 'runtime_capabilities', runtime_capabilities, 'software_version', software_version, 'created_at', created_at, 'updated_at', updated_at, 'desired_revision', desired_revision)`
const administrationJSON = `jsonb_build_object('agent_id', agent_id, 'lifecycle_state', lifecycle_state, 'operator_intent', operator_intent, 'maintenance_message', maintenance_message, 'credential_revoked_at', credential_revoked_at, 'updated_at', updated_at)`
const environmentJSON = `jsonb_build_object('id', id, 'project_id', project_id, 'name', name, 'kind', kind, 'is_production', is_production, 'network_identity', network_identity, 'copied_from_environment_id', copied_from_environment_id, 'created_at', created_at, 'updated_at', updated_at)`
const volumeJSON = `jsonb_build_object('id', id, 'environment_id', environment_id, 'name', name, 'size_bytes', size_bytes, 'created_at', created_at)`
const domainJSON = `jsonb_build_object('hostname', hostname, 'service_id', service_id, 'target_port', target_port, 'platform_generated', platform_generated, 'created_at', created_at, 'updated_at', updated_at)`

var productTables = []productTable{
	{
		name: "projects", keySQL: "id", jsonSQL: projectJSON,
		decode: func(s *DurableState, key string, raw []byte) error { return decodeRecord(&s.Projects, key, raw) },
		resolve: func(ctx context.Context, tx *sql.Tx, base DurableState, keys []string) (Batch, error) {
			changes, err := resolveChanges(ctx, tx, base.Projects, keys, keyedRead[Project]{
				name: "projects", keySQL: "id", jsonSQL: projectJSON,
				decode: decodeValue[Project], where: singleKeyWhereID,
			})
			return Batch{Projects: changes}, err
		},
	},
	{
		name: "services", keySQL: "id::STRING", jsonSQL: serviceJSON,
		decode: func(s *DurableState, key string, raw []byte) error { return decodeRecord(&s.Services, key, raw) },
		resolve: func(ctx context.Context, tx *sql.Tx, base DurableState, keys []string) (Batch, error) {
			changes, err := resolveChanges(ctx, tx, base.Services, keys, keyedRead[ServiceIntent]{
				name: "services", keySQL: "id::STRING", jsonSQL: serviceJSON,
				decode: decodeValue[ServiceIntent], where: singleKeyWhereID,
			})
			return Batch{Services: changes}, err
		},
	},
	{
		name: "service_revisions", keySQL: "service_id::STRING || '/' || spec_revision::STRING", jsonSQL: revisionJSON,
		decode: func(s *DurableState, key string, raw []byte) error { return decodeRecord(&s.Revisions, key, raw) },
		resolve: func(ctx context.Context, tx *sql.Tx, base DurableState, keys []string) (Batch, error) {
			changes, err := resolveChanges(ctx, tx, base.Revisions, keys, keyedRead[ServiceRevision]{
				name: "service_revisions", keySQL: "service_id::STRING || '/' || spec_revision::STRING", jsonSQL: revisionJSON,
				decode: decodeValue[ServiceRevision], where: pairKeyWhereRevisions,
			})
			return Batch{Revisions: changes}, err
		},
	},
	{
		name: "allocation_assignments", keySQL: "id::STRING", jsonSQL: assignmentJSON,
		decode: func(s *DurableState, key string, raw []byte) error { return decodeRecord(&s.Assignments, key, raw) },
		resolve: func(ctx context.Context, tx *sql.Tx, base DurableState, keys []string) (Batch, error) {
			changes, err := resolveChanges(ctx, tx, base.Assignments, keys, keyedRead[Assignment]{
				name: "allocation_assignments", keySQL: "id::STRING", jsonSQL: assignmentJSON,
				decode: decodeValue[Assignment], where: singleKeyWhereID,
			})
			return Batch{Assignments: changes}, err
		},
	},
	{
		name: "service_rollouts", keySQL: "service_id::STRING || '/' || rollout_generation::STRING", jsonSQL: rolloutJSON,
		decode: func(s *DurableState, key string, raw []byte) error { return decodeRecord(&s.Rollouts, key, raw) },
		resolve: func(ctx context.Context, tx *sql.Tx, base DurableState, keys []string) (Batch, error) {
			changes, err := resolveChanges(ctx, tx, base.Rollouts, keys, keyedRead[Rollout]{
				name: "service_rollouts", keySQL: "service_id::STRING || '/' || rollout_generation::STRING", jsonSQL: rolloutJSON,
				decode: decodeValue[Rollout], where: pairKeyWhereRollouts,
			})
			return Batch{Rollouts: changes}, err
		},
	},
	{
		name: "deployments", keySQL: "id::STRING", jsonSQL: deploymentJSON,
		decode: func(s *DurableState, key string, raw []byte) error { return decodeRecord(&s.Deployments, key, raw) },
		resolve: func(ctx context.Context, tx *sql.Tx, base DurableState, keys []string) (Batch, error) {
			changes, err := resolveChanges(ctx, tx, base.Deployments, keys, keyedRead[Deployment]{
				name: "deployments", keySQL: "id::STRING", jsonSQL: deploymentJSON,
				decode: decodeValue[Deployment], where: singleKeyWhereID,
			})
			return Batch{Deployments: changes}, err
		},
	},
	{
		name: "agent_registrations", keySQL: "id::STRING", jsonSQL: agentJSON,
		decode: func(s *DurableState, key string, raw []byte) error { return decodeRecord(&s.Agents, key, raw) },
		resolve: func(ctx context.Context, tx *sql.Tx, base DurableState, keys []string) (Batch, error) {
			changes, err := resolveChanges(ctx, tx, base.Agents, keys, keyedRead[AgentRegistration]{
				name: "agent_registrations", keySQL: "id::STRING", jsonSQL: agentJSON,
				decode: decodeValue[AgentRegistration], where: singleKeyWhereID,
			})
			return Batch{Agents: changes}, err
		},
	},
	{
		name: "agent_administration", keySQL: "agent_id::STRING", jsonSQL: administrationJSON,
		decode: func(s *DurableState, key string, raw []byte) error { return decodeRecord(&s.Administration, key, raw) },
		resolve: func(ctx context.Context, tx *sql.Tx, base DurableState, keys []string) (Batch, error) {
			changes, err := resolveChanges(ctx, tx, base.Administration, keys, keyedRead[AgentAdministration]{
				name: "agent_administration", keySQL: "agent_id::STRING", jsonSQL: administrationJSON,
				decode: decodeValue[AgentAdministration], where: singleKeyWhereAgentID,
			})
			return Batch{Administration: changes}, err
		},
	},
	{
		name: "environments", keySQL: "id::STRING", jsonSQL: environmentJSON,
		decode: func(s *DurableState, key string, raw []byte) error { return decodeRecord(&s.Environments, key, raw) },
		resolve: func(ctx context.Context, tx *sql.Tx, base DurableState, keys []string) (Batch, error) {
			changes, err := resolveChanges(ctx, tx, base.Environments, keys, keyedRead[Environment]{
				name: "environments", keySQL: "id::STRING", jsonSQL: environmentJSON,
				decode: decodeValue[Environment], where: singleKeyWhereID,
			})
			return Batch{Environments: changes}, err
		},
	},
	{
		name: "volumes", keySQL: "id::STRING", jsonSQL: volumeJSON,
		decode: func(s *DurableState, key string, raw []byte) error { return decodeRecord(&s.Volumes, key, raw) },
		resolve: func(ctx context.Context, tx *sql.Tx, base DurableState, keys []string) (Batch, error) {
			changes, err := resolveChanges(ctx, tx, base.Volumes, keys, keyedRead[Volume]{
				name: "volumes", keySQL: "id::STRING", jsonSQL: volumeJSON,
				decode: decodeValue[Volume], where: singleKeyWhereID,
			})
			return Batch{Volumes: changes}, err
		},
	},
	{
		name: "domain_bindings", keySQL: "hostname::STRING", jsonSQL: domainJSON,
		decode: func(s *DurableState, key string, raw []byte) error { return decodeRecord(&s.Domains, key, raw) },
		resolve: func(ctx context.Context, tx *sql.Tx, base DurableState, keys []string) (Batch, error) {
			changes, err := resolveChanges(ctx, tx, base.Domains, keys, keyedRead[Domain]{
				name: "domain_bindings", keySQL: "hostname::STRING", jsonSQL: domainJSON,
				decode: decodeValue[Domain], where: singleKeyWhereHostname,
			})
			return Batch{Domains: changes}, err
		},
	},
}

func singleKeyWhereID(keys []string) (string, []any) {
	return singleKeyWhere("id", keys)
}

func singleKeyWhereAgentID(keys []string) (string, []any) {
	return singleKeyWhere("agent_id", keys)
}

func singleKeyWhereHostname(keys []string) (string, []any) {
	return singleKeyWhere("hostname", keys)
}

func pairKeyWhereRevisions(keys []string) (string, []any) {
	return pairKeyWhere("service_id", "spec_revision", keys)
}

func pairKeyWhereRollouts(keys []string) (string, []any) {
	return pairKeyWhere("service_id", "rollout_generation", keys)
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
	return value, nil
}
