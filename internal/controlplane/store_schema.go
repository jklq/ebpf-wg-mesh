package controlplane

const currentSchemaVersion = 4

var schemaUpgrades = map[int][]string{
	2: {
		`ALTER TABLE allocations ADD COLUMN IF NOT EXISTS restart_observation_json JSONB NOT NULL DEFAULT '{}'`,
		`ALTER TABLE allocations ADD COLUMN IF NOT EXISTS operator_restart_nonce INT8 NOT NULL DEFAULT 0`,
	},
	3: {
		`ALTER TABLE services ADD COLUMN IF NOT EXISTS desired_replica_count INT8 NOT NULL DEFAULT 1`,
		`ALTER TABLE services ADD COLUMN IF NOT EXISTS placement_message STRING NOT NULL DEFAULT ''`,
		`ALTER TABLE allocations ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ NOT NULL DEFAULT now()`,
		`ALTER TABLE allocations ALTER COLUMN created_at DROP DEFAULT`,
		`ALTER TABLE allocations DROP CONSTRAINT IF EXISTS allocations_service_id_key`,
		`CREATE INDEX IF NOT EXISTS idx_allocations_service ON allocations(service_id, id)`,
	},
	4: {
		`ALTER TABLE allocations ADD COLUMN IF NOT EXISTS rollout_state STRING NOT NULL DEFAULT 'serving'`,
		`ALTER TABLE allocations ADD COLUMN IF NOT EXISTS drain_started_at TIMESTAMPTZ NULL`,
		`ALTER TABLE allocations ADD COLUMN IF NOT EXISTS drain_deadline TIMESTAMPTZ NULL`,
		`ALTER TABLE service_rollouts ADD COLUMN IF NOT EXISTS state STRING NOT NULL DEFAULT 'succeeded'`,
		`ALTER TABLE service_rollouts ADD COLUMN IF NOT EXISTS strategy_json JSONB NOT NULL DEFAULT '{}'`,
		`ALTER TABLE service_rollouts ADD COLUMN IF NOT EXISTS desired_replica_count INT8 NOT NULL DEFAULT 1`,
		`ALTER TABLE service_rollouts ADD COLUMN IF NOT EXISTS image_digest STRING NOT NULL DEFAULT ''`,
		`ALTER TABLE service_rollouts ADD COLUMN IF NOT EXISTS failure_reason STRING NOT NULL DEFAULT ''`,
		`ALTER TABLE service_rollouts ADD COLUMN IF NOT EXISTS completed_at TIMESTAMPTZ NULL`,
		`ALTER TABLE service_rollouts ADD COLUMN IF NOT EXISTS progress_at TIMESTAMPTZ NOT NULL DEFAULT now()`,
		`CREATE INDEX IF NOT EXISTS idx_service_rollouts_in_progress ON service_rollouts(state, created_at, service_id)`,
	},
}

var currentSchema = []string{
	`CREATE TABLE projects (
			id STRING PRIMARY KEY,
			name STRING NOT NULL,
			kind STRING NOT NULL,
			system_key STRING NULL,
			owner_user_id STRING NOT NULL,
			created_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE UNIQUE INDEX idx_projects_owner_name
			ON projects(owner_user_id, name) WHERE kind = 'user'`,
	`CREATE UNIQUE INDEX idx_projects_system_key
			ON projects(system_key) WHERE system_key IS NOT NULL`,
	`CREATE TABLE project_memberships (
			user_id STRING NOT NULL,
			project_id STRING NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			role STRING NOT NULL,
			PRIMARY KEY (user_id, project_id)
		)`,
	`CREATE INDEX idx_project_memberships_user ON project_memberships(user_id, project_id)`,
	`CREATE TABLE environment_network_identity_counter (
			id BOOL PRIMARY KEY,
			next_identity INT8 NOT NULL
		)`,
	`INSERT INTO environment_network_identity_counter(id, next_identity) VALUES (TRUE, 1)`,
	`CREATE TABLE environments (
			id STRING PRIMARY KEY,
			project_id STRING NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			name STRING NOT NULL,
			kind STRING NOT NULL,
			is_production BOOL NOT NULL DEFAULT FALSE,
			network_identity INT8 NOT NULL UNIQUE,
			copied_from_environment_id STRING NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			UNIQUE (project_id, name)
		)`,
	`CREATE UNIQUE INDEX idx_environments_one_production
			ON environments(project_id) WHERE is_production = TRUE`,
	`CREATE INDEX idx_environments_project_created ON environments(project_id, created_at, id)`,
	`CREATE TABLE agents (
			id STRING PRIMARY KEY,
			name STRING NOT NULL,
			advertise_addr STRING NOT NULL,
			workload_ipv6_subnet STRING NOT NULL DEFAULT '',
			wireguard_public_key STRING NOT NULL DEFAULT '',
			wireguard_listen_port INT8 NOT NULL DEFAULT 0,
			wireguard_ipv6 STRING NOT NULL DEFAULT '',
			cpu_millis_capacity INT8 NOT NULL,
			memory_mebibytes_capacity INT8 NOT NULL,
			last_seen_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			desired_revision INT8 NOT NULL DEFAULT 0
		)`,
	`CREATE INDEX idx_agents_last_seen_id ON agents(last_seen_at DESC, id)
			STORING (cpu_millis_capacity, memory_mebibytes_capacity)`,
	`CREATE TABLE agent_bootstrap_tokens (
			token_hash BYTES PRIMARY KEY,
			agent_id STRING NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			consumed_at TIMESTAMPTZ NULL
		)`,
	`CREATE INDEX idx_agent_bootstrap_tokens_agent ON agent_bootstrap_tokens(agent_id, consumed_at)`,
	`CREATE TABLE volumes (
			id STRING PRIMARY KEY,
			environment_id STRING NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
			name STRING NOT NULL,
			size_bytes INT8 NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			UNIQUE (environment_id, name)
		)`,
	`CREATE INDEX idx_volumes_environment_created ON volumes(environment_id, created_at, id)`,
	`CREATE TABLE services (
			id STRING PRIMARY KEY,
			environment_id STRING NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
			name STRING NOT NULL,
			current_spec_revision INT8 NOT NULL,
			current_rollout_generation INT8 NOT NULL DEFAULT 0,
			current_resolved_image STRING NOT NULL DEFAULT '',
			last_successful_commit_sha STRING NOT NULL DEFAULT '',
			latest_build_id STRING NOT NULL DEFAULT '',
			desired_replica_count INT8 NOT NULL DEFAULT 1,
			placement_message STRING NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			UNIQUE (environment_id, name)
		)`,
	`CREATE INDEX idx_services_environment_created ON services(environment_id, created_at, id)`,
	`CREATE TABLE service_revisions (
			service_id STRING NOT NULL REFERENCES services(id) ON DELETE CASCADE,
			spec_revision INT8 NOT NULL,
			spec_json JSONB NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (service_id, spec_revision)
		)`,
	`CREATE TABLE domain_bindings (
			hostname STRING PRIMARY KEY,
			service_id STRING NOT NULL REFERENCES services(id) ON DELETE CASCADE,
			target_port INT8 NOT NULL DEFAULT 8080,
			platform_generated BOOL NOT NULL DEFAULT FALSE,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE INDEX idx_domain_bindings_service ON domain_bindings(service_id, hostname)`,
	`CREATE UNIQUE INDEX idx_domain_bindings_generated_service
			ON domain_bindings(service_id) WHERE platform_generated = TRUE`,
	`CREATE TABLE allocations (
			id STRING PRIMARY KEY,
			service_id STRING NOT NULL REFERENCES services(id) ON DELETE CASCADE,
			agent_id STRING NOT NULL REFERENCES agents(id),
			desired_spec_revision INT8 NOT NULL,
			applied_spec_revision INT8 NOT NULL,
			desired_rollout_generation INT8 NOT NULL,
			applied_rollout_generation INT8 NOT NULL,
			phase STRING NOT NULL,
			message STRING NOT NULL,
			allocation_ip STRING NOT NULL DEFAULT '',
			healthy_ports JSONB NOT NULL DEFAULT '[]',
			healthy BOOL NOT NULL,
			restart_observation_json JSONB NOT NULL DEFAULT '{}',
			operator_restart_nonce INT8 NOT NULL DEFAULT 0,
			rollout_state STRING NOT NULL,
			drain_started_at TIMESTAMPTZ NULL,
			drain_deadline TIMESTAMPTZ NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE INDEX idx_allocations_agent ON allocations(agent_id, updated_at, id)`,
	`CREATE INDEX idx_allocations_service ON allocations(service_id, id)`,
	`CREATE TABLE service_rollouts (
			service_id STRING NOT NULL REFERENCES services(id) ON DELETE CASCADE,
			rollout_generation INT8 NOT NULL,
			spec_revision INT8 NOT NULL,
			reason STRING NOT NULL,
			build_id STRING NOT NULL DEFAULT '',
			requested_by_user_id STRING NOT NULL DEFAULT '',
			state STRING NOT NULL,
			strategy_json JSONB NOT NULL,
			desired_replica_count INT8 NOT NULL,
			image_digest STRING NOT NULL DEFAULT '',
			failure_reason STRING NOT NULL DEFAULT '',
			completed_at TIMESTAMPTZ NULL,
			progress_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (service_id, rollout_generation)
		)`,
	`CREATE INDEX idx_service_rollouts_in_progress ON service_rollouts(state, created_at, service_id)`,
	`CREATE TABLE deployments (
			id STRING PRIMARY KEY,
			service_id STRING NOT NULL REFERENCES services(id) ON DELETE CASCADE,
			spec_revision INT8 NOT NULL,
			rollout_generation INT8 NOT NULL DEFAULT 0,
			build_id STRING NOT NULL DEFAULT '',
			image_digest STRING NOT NULL DEFAULT '',
			state STRING NOT NULL,
			cause_kind STRING NOT NULL,
			cause_id STRING NOT NULL DEFAULT '',
			reason_code STRING NOT NULL,
			detail STRING NOT NULL DEFAULT '',
			is_current BOOL NOT NULL DEFAULT FALSE,
			requested_by_user_id STRING NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE UNIQUE INDEX idx_deployments_service_current
			ON deployments(service_id) WHERE is_current = TRUE`,
	`CREATE UNIQUE INDEX idx_deployments_service_build
			ON deployments(service_id, build_id) WHERE build_id != ''`,
	`CREATE INDEX idx_deployments_service_rollout
			ON deployments(service_id, rollout_generation DESC, created_at DESC, id)`,
	`CREATE INDEX idx_deployments_service_updated
			ON deployments(service_id, updated_at DESC, id)`,
	`CREATE TABLE deployment_transitions (
			id STRING PRIMARY KEY,
			deployment_id STRING NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
			from_state STRING NOT NULL,
			to_state STRING NOT NULL,
			cause_kind STRING NOT NULL,
			cause_id STRING NOT NULL DEFAULT '',
			reason_code STRING NOT NULL,
			detail STRING NOT NULL DEFAULT '',
			spec_revision INT8 NOT NULL,
			image_digest STRING NOT NULL DEFAULT '',
			rollout_generation INT8 NOT NULL DEFAULT 0,
			occurred_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE INDEX idx_deployment_transitions_deployment
			ON deployment_transitions(deployment_id, occurred_at ASC, id)`,
	`CREATE TABLE builder_workers (
			id STRING PRIMARY KEY,
			name STRING NOT NULL,
			current_build_id STRING NOT NULL DEFAULT '',
			last_heartbeat_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE TABLE build_runs (
			id STRING PRIMARY KEY,
			service_id STRING NOT NULL REFERENCES services(id) ON DELETE CASCADE,
			commit_sha STRING NOT NULL,
			commit_message STRING NOT NULL DEFAULT '',
			commit_author STRING NOT NULL DEFAULT '',
			state STRING NOT NULL,
			builder_id STRING NOT NULL DEFAULT '',
			image_digest STRING NOT NULL DEFAULT '',
			failure_reason STRING NOT NULL DEFAULT '',
			source_revision_id STRING NOT NULL DEFAULT '',
			source_snapshot_id STRING NOT NULL DEFAULT '',
			source_snapshot_digest STRING NOT NULL DEFAULT '',
			build_recipe_json JSONB NOT NULL DEFAULT '{}',
			target_rollout_generation INT8 NOT NULL DEFAULT 0,
			queued_at TIMESTAMPTZ NOT NULL,
			started_at TIMESTAMPTZ NULL,
			finished_at TIMESTAMPTZ NULL
		)`,
	`CREATE INDEX idx_build_runs_service_queued ON build_runs(service_id, queued_at DESC, id)`,
	`CREATE INDEX idx_build_runs_service_commit ON build_runs(service_id, commit_sha, queued_at DESC, id DESC)`,
	`CREATE INDEX idx_build_runs_state_queued ON build_runs(state, queued_at ASC, id)`,
	`CREATE TABLE github_installations (
			installation_id INT8 PRIMARY KEY,
			account_login STRING NOT NULL,
			account_type STRING NOT NULL,
			target_type STRING NOT NULL,
			active BOOL NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE TABLE github_installation_repositories (
			installation_id INT8 NOT NULL REFERENCES github_installations(installation_id) ON DELETE CASCADE,
			repository_id INT8 NOT NULL,
			owner STRING NOT NULL,
			repo STRING NOT NULL,
			full_name STRING NOT NULL,
			private BOOL NOT NULL,
			default_branch STRING NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (installation_id, repository_id),
			UNIQUE (installation_id, full_name)
		)`,
	`CREATE INDEX idx_github_installation_repositories_name
			ON github_installation_repositories(full_name, installation_id)`,
	`CREATE TABLE project_github_repositories (
			project_id STRING NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			installation_id INT8 NOT NULL,
			repository_id INT8 NOT NULL,
			full_name STRING NOT NULL,
			linked_by_user_id STRING NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (project_id, repository_id),
			UNIQUE (project_id, full_name)
		)`,
	`CREATE INDEX idx_project_github_repositories_installation
			ON project_github_repositories(installation_id, repository_id, project_id)`,
	`CREATE TABLE github_repository_snapshots (
			full_name STRING PRIMARY KEY,
			repository_id INT8 NOT NULL DEFAULT 0,
			owner STRING NOT NULL,
			repo STRING NOT NULL,
			private BOOL NOT NULL DEFAULT FALSE,
			default_branch STRING NOT NULL DEFAULT '',
			deleted BOOL NOT NULL DEFAULT FALSE,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE INDEX idx_github_repository_snapshots_owner_repo
			ON github_repository_snapshots(owner, repo)`,
	`CREATE TABLE github_webhook_deliveries (
			id STRING PRIMARY KEY,
			delivery_id STRING NOT NULL UNIQUE,
			event_type STRING NOT NULL,
			state STRING NOT NULL,
			processor_id STRING NOT NULL DEFAULT '',
			payload JSONB NOT NULL,
			last_error STRING NOT NULL DEFAULT '',
			received_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			processed_at TIMESTAMPTZ NULL
		)`,
	`CREATE INDEX idx_github_webhook_deliveries_state
			ON github_webhook_deliveries(state, received_at ASC, id)`,
	`CREATE TABLE github_work_items (
			id STRING PRIMARY KEY,
			kind STRING NOT NULL,
			state STRING NOT NULL,
			processor_id STRING NOT NULL DEFAULT '',
			idempotency_key STRING NOT NULL UNIQUE,
			service_id STRING NOT NULL DEFAULT '',
			spec_revision INT8 NOT NULL DEFAULT 0,
			installation_id INT8 NOT NULL DEFAULT 0,
			commit_sha STRING NOT NULL DEFAULT '',
			last_error STRING NOT NULL DEFAULT '',
			attempt_count INT8 NOT NULL DEFAULT 0,
			available_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE INDEX idx_github_work_items_state ON github_work_items(state, available_at ASC, created_at ASC, id)`,
	`CREATE TABLE source_bindings (
			id STRING PRIMARY KEY,
			service_id STRING NOT NULL UNIQUE REFERENCES services(id) ON DELETE CASCADE,
			provider STRING NOT NULL,
			repository_selector STRING NOT NULL DEFAULT '',
			tracked_ref STRING NOT NULL DEFAULT '',
			provider_repository_external_id STRING NOT NULL DEFAULT '',
			provider_scope_external_id STRING NOT NULL DEFAULT '',
			access_state STRING NOT NULL DEFAULT '',
			build_recipe_json JSONB NOT NULL DEFAULT '{}',
			resolved_at TIMESTAMPTZ NOT NULL,
			fresh_until TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE INDEX idx_source_bindings_provider_repo_ref
			ON source_bindings(provider, provider_repository_external_id, tracked_ref, service_id)`,
	`CREATE INDEX idx_source_bindings_provider_scope
			ON source_bindings(provider, provider_scope_external_id, service_id)`,
	`CREATE TABLE source_revisions (
			id STRING PRIMARY KEY,
			source_binding_id STRING NOT NULL REFERENCES source_bindings(id) ON DELETE CASCADE,
			service_id STRING NOT NULL REFERENCES services(id) ON DELETE CASCADE,
			provider STRING NOT NULL,
			provider_repository_external_id STRING NOT NULL DEFAULT '',
			tracked_ref STRING NOT NULL DEFAULT '',
			commit_sha STRING NOT NULL,
			commit_message STRING NOT NULL DEFAULT '',
			commit_author STRING NOT NULL DEFAULT '',
			observed_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			UNIQUE (source_binding_id, commit_sha)
		)`,
	`CREATE INDEX idx_source_revisions_service_observed ON source_revisions(service_id, observed_at DESC, id)`,
	`CREATE INDEX idx_source_revisions_provider_repo_commit
			ON source_revisions(provider, provider_repository_external_id, commit_sha, id)`,
	`CREATE TABLE source_snapshots (
			id STRING PRIMARY KEY,
			source_revision_id STRING NOT NULL DEFAULT '' UNIQUE,
			provider STRING NOT NULL,
			provider_repository_external_id STRING NOT NULL DEFAULT '',
			commit_sha STRING NOT NULL,
			digest STRING NOT NULL DEFAULT '',
			object_key STRING NOT NULL DEFAULT '',
			archive_size_bytes INT8 NOT NULL DEFAULT 0,
			ready BOOL NOT NULL DEFAULT FALSE,
			fetched_at TIMESTAMPTZ NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			UNIQUE (provider, provider_repository_external_id, commit_sha)
		)`,
	`CREATE INDEX idx_source_snapshots_created ON source_snapshots(created_at, id)`,
	`CREATE TABLE source_work_items (
			id STRING PRIMARY KEY,
			kind STRING NOT NULL,
			state STRING NOT NULL,
			processor_id STRING NOT NULL DEFAULT '',
			idempotency_key STRING NOT NULL UNIQUE,
			service_id STRING NOT NULL DEFAULT '',
			spec_revision INT8 NOT NULL DEFAULT 0,
			provider STRING NOT NULL DEFAULT '',
			provider_repository_external_id STRING NOT NULL DEFAULT '',
			provider_scope_external_id STRING NOT NULL DEFAULT '',
			tracked_ref STRING NOT NULL DEFAULT '',
			commit_sha STRING NOT NULL DEFAULT '',
			commit_message STRING NOT NULL DEFAULT '',
			commit_author STRING NOT NULL DEFAULT '',
			last_error STRING NOT NULL DEFAULT '',
			attempt_count INT8 NOT NULL DEFAULT 0,
			available_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE INDEX idx_source_work_items_state ON source_work_items(state, available_at ASC, created_at ASC, id)`,
}
