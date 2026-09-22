package controlplane

const currentSchemaVersion = 28

// currentSchema contains both owned relational references and retained external
// identifiers. User IDs, GitHub repository links, and certificate enrollment
// records intentionally have no foreign key: their owning system or lifecycle
// is outside the referenced control-plane row.
var currentSchema = []string{
	`CREATE TABLE cluster_journal_heads (
		cluster_id STRING PRIMARY KEY,
		log_index INT8 NOT NULL CHECK (log_index >= 0),
		compacted_index INT8 NOT NULL DEFAULT 0 CHECK (compacted_index >= 0 AND compacted_index <= log_index)
	)`,
	`INSERT INTO cluster_journal_heads(cluster_id, log_index, compacted_index) VALUES ('default', 0, 0)`,
	`CREATE TABLE cluster_journal (
		cluster_id STRING NOT NULL REFERENCES cluster_journal_heads(cluster_id),
		log_index INT8 NOT NULL CHECK (log_index > 0),
		command_id STRING NOT NULL,
		command_version INT8 NOT NULL,
		command_type STRING NOT NULL,
		payload JSONB NOT NULL,
		authorizing_epoch INT8 NULL CHECK (authorizing_epoch > 0),
		created_at TIMESTAMPTZ NOT NULL,
		PRIMARY KEY (cluster_id, log_index),
		UNIQUE (cluster_id, command_id)
	)`,
	`CREATE TABLE cluster_journal_receipts (
		cluster_id STRING NOT NULL REFERENCES cluster_journal_heads(cluster_id),
		command_id STRING NOT NULL,
		log_index INT8 NOT NULL CHECK (log_index >= 0),
		command_version INT8 NOT NULL,
		command_type STRING NOT NULL,
		payload JSONB NOT NULL,
		authorizing_epoch INT8 NULL CHECK (authorizing_epoch > 0),
		created_at TIMESTAMPTZ NOT NULL,
		expires_at TIMESTAMPTZ NOT NULL,
		PRIMARY KEY (cluster_id, command_id)
	)`,
	`CREATE INDEX idx_cluster_journal_receipts_expiry ON cluster_journal_receipts(cluster_id, expires_at)`,
	`CREATE TABLE control_plane_leases (
			name STRING PRIMARY KEY,
			holder_id STRING NOT NULL,
			fencing_token INT8 NOT NULL,
			advertise_addr STRING NOT NULL DEFAULT '',
			expires_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE TABLE environment_events (
			environment_id STRING PRIMARY KEY,
			revision INT8 NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE TABLE control_plane_storage (
			name STRING PRIMARY KEY,
			storage_id STRING NOT NULL,
			created_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE TABLE projects (
			id STRING PRIMARY KEY,
			name STRING NOT NULL,
			kind STRING NOT NULL,
			system_key STRING NULL,
			owner_user_id STRING NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			deleted_at TIMESTAMPTZ NULL,
			deleted_by_user_id STRING NOT NULL DEFAULT '',
			delete_expires_at TIMESTAMPTZ NULL
		)`,
	`CREATE INDEX idx_projects_delete_expires ON projects(delete_expires_at, id) WHERE deleted_at IS NOT NULL`,
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
	`CREATE TABLE platform_operators (
			user_id STRING PRIMARY KEY,
			created_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE TABLE environment_network_identity_counter (
			id BOOL PRIMARY KEY,
			next_identity INT8 NOT NULL
		)`,
	`INSERT INTO environment_network_identity_counter(id, next_identity) VALUES (TRUE, 1)`,
	`CREATE TABLE workload_ipv4_prefix_allocator (
			id BOOL PRIMARY KEY,
			pool_cidr STRING NOT NULL,
			prefix_bits INT8 NOT NULL,
			next_ordinal INT8 NOT NULL
		)`,
	`CREATE TABLE environments (
			id STRING PRIMARY KEY,
			project_id STRING NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			name STRING NOT NULL,
			kind STRING NOT NULL,
			is_production BOOL NOT NULL DEFAULT FALSE,
			auto_deploy BOOL NOT NULL,
			network_identity INT8 NOT NULL UNIQUE,
			copied_from_environment_id STRING NULL REFERENCES environments(id) ON DELETE SET NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			deleted_at TIMESTAMPTZ NULL,
			deleted_by_user_id STRING NOT NULL DEFAULT '',
			delete_expires_at TIMESTAMPTZ NULL,
			UNIQUE (project_id, name)
		)`,
	`CREATE INDEX idx_environments_delete_expires ON environments(delete_expires_at, id) WHERE deleted_at IS NOT NULL`,
	`CREATE UNIQUE INDEX idx_environments_one_production
			ON environments(project_id) WHERE is_production = TRUE`,
	`CREATE INDEX idx_environments_project_created ON environments(project_id, created_at, id)`,
	`CREATE TABLE agent_authority (
 id INT8 PRIMARY KEY CHECK (id = 1), epoch INT8 NOT NULL CHECK (epoch > 0),
 outstanding_not_after TIMESTAMPTZ NOT NULL
 )`,
	`INSERT INTO agent_authority VALUES (1, 1, '1970-01-01')`,
	`CREATE TABLE agent_registrations (
			id STRING PRIMARY KEY,
			name STRING NOT NULL,
 local_store_id STRING NOT NULL DEFAULT '',
 session_incarnation INT8 NOT NULL DEFAULT 0,
			region STRING NOT NULL,
			zone STRING NOT NULL DEFAULT '',
			failure_domain STRING NOT NULL,
			reserved_cpu_millis INT8 NOT NULL DEFAULT 0,
			reserved_memory_mebibytes INT8 NOT NULL DEFAULT 0,
			advertise_addr STRING NOT NULL DEFAULT '',
			workload_ipv4_subnet STRING NOT NULL DEFAULT '',
			workload_ipv6_subnet STRING NOT NULL DEFAULT '',
			wireguard_public_key STRING NOT NULL DEFAULT '',
			wireguard_listen_port INT8 NOT NULL DEFAULT 0,
			wireguard_endpoint STRING NOT NULL DEFAULT '',
			wireguard_ipv6 STRING NOT NULL DEFAULT '',
			cpu_millis_capacity INT8 NOT NULL DEFAULT 0,
			memory_mebibytes_capacity INT8 NOT NULL DEFAULT 0,
			runtime_capabilities JSONB NOT NULL DEFAULT '[]',
			software_version STRING NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			desired_revision INT8 NOT NULL DEFAULT 0
		)`,
	`CREATE UNIQUE INDEX idx_agent_registrations_workload_ipv4_subnet ON agent_registrations(workload_ipv4_subnet) WHERE workload_ipv4_subnet <> ''`,
	`CREATE TABLE agent_administration (
			agent_id STRING PRIMARY KEY REFERENCES agent_registrations(id) ON DELETE CASCADE,
			lifecycle_state STRING NOT NULL,
			operator_intent STRING NOT NULL DEFAULT '',
			maintenance_message STRING NOT NULL DEFAULT '',
			credential_revoked_at TIMESTAMPTZ NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE VIEW agents AS SELECT r.id, r.name,
			ad.lifecycle_state,
			ad.lifecycle_state AS state_before_unavailable,
			r.region, r.zone, r.failure_domain, r.reserved_cpu_millis, r.reserved_memory_mebibytes,
			r.advertise_addr, r.workload_ipv4_subnet, r.workload_ipv6_subnet, r.wireguard_public_key,
			r.wireguard_listen_port, r.wireguard_endpoint, r.wireguard_ipv6, r.cpu_millis_capacity, r.memory_mebibytes_capacity,
			r.runtime_capabilities, r.software_version, ad.maintenance_message, ad.credential_revoked_at,
			r.created_at AS last_seen_at, r.created_at, GREATEST(r.updated_at, ad.updated_at) AS updated_at,
			r.desired_revision
		FROM agent_registrations r
		JOIN agent_administration ad ON ad.agent_id = r.id`,
	`CREATE TABLE agent_bootstrap_tokens (
			token_hash BYTES PRIMARY KEY,
			agent_id STRING NOT NULL,
			origin STRING NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			consumed_at TIMESTAMPTZ NULL,
			csr_public_key_sha256 BYTES NULL
		)`,
	`CREATE INDEX idx_agent_bootstrap_tokens_agent ON agent_bootstrap_tokens(agent_id, consumed_at)`,
	`CREATE TABLE agent_certificates (
			serial STRING PRIMARY KEY,
			agent_id STRING NOT NULL,
			issued_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE INDEX idx_agent_certificates_agent ON agent_certificates(agent_id, issued_at DESC)`,
	`CREATE TABLE volumes (
			id STRING PRIMARY KEY,
			environment_id STRING NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
			name STRING NOT NULL CHECK (btrim(name) <> ''),
			size_bytes INT8 NOT NULL CHECK (size_bytes > 0),
			created_at TIMESTAMPTZ NOT NULL,
			deleted_at TIMESTAMPTZ NULL,
			deleted_by_user_id STRING NOT NULL DEFAULT '',
			delete_expires_at TIMESTAMPTZ NULL,
			UNIQUE (environment_id, name)
		)`,
	`CREATE INDEX idx_volumes_delete_expires ON volumes(delete_expires_at, id) WHERE deleted_at IS NOT NULL`,
	`CREATE INDEX idx_volumes_environment_created ON volumes(environment_id, created_at, id)`,
	`CREATE TABLE services (
			id STRING PRIMARY KEY,
			environment_id STRING NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
			name STRING NOT NULL,
			current_spec_revision INT8 NOT NULL,
			desired_replica_count INT8 NOT NULL DEFAULT 1,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			deleted_at TIMESTAMPTZ NULL,
			deleted_by_user_id STRING NOT NULL DEFAULT '',
			delete_expires_at TIMESTAMPTZ NULL,
			UNIQUE (environment_id, name)
		)`,
	`CREATE INDEX idx_services_delete_expires ON services(delete_expires_at, id) WHERE deleted_at IS NOT NULL`,
	`CREATE INDEX idx_services_environment_created ON services(environment_id, created_at, id)`,
	`CREATE TABLE service_revisions (
			service_id STRING NOT NULL REFERENCES services(id) ON DELETE CASCADE,
			spec_revision INT8 NOT NULL,
			spec_json JSONB NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (service_id, spec_revision)
	)`,
	`CREATE TABLE service_delivery_status (
			service_id STRING PRIMARY KEY REFERENCES services(id) ON DELETE CASCADE,
			current_rollout_generation INT8 NULL,
			current_resolved_image STRING NULL,
			last_successful_commit_sha STRING NULL,
			latest_build_id STRING NULL,
			placement_message STRING NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE TABLE domain_bindings (
			hostname STRING PRIMARY KEY,
			service_id STRING NOT NULL REFERENCES services(id) ON DELETE CASCADE,
			target_port INT8 NOT NULL DEFAULT 8080,
			platform_generated BOOL NOT NULL DEFAULT FALSE,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			deleted_at TIMESTAMPTZ NULL,
			deleted_by_user_id STRING NOT NULL DEFAULT '',
			delete_expires_at TIMESTAMPTZ NULL
		)`,
	`CREATE INDEX idx_domain_bindings_delete_expires ON domain_bindings(delete_expires_at, hostname) WHERE deleted_at IS NOT NULL`,
	`CREATE INDEX idx_domain_bindings_service ON domain_bindings(service_id, hostname)`,
	`CREATE UNIQUE INDEX idx_domain_bindings_generated_service
			ON domain_bindings(service_id) WHERE platform_generated = TRUE`,
	`CREATE TABLE allocation_assignments (
			id STRING PRIMARY KEY,
			service_id STRING NOT NULL REFERENCES services(id) ON DELETE CASCADE,
			deployment_id STRING NOT NULL,
			agent_id STRING NOT NULL REFERENCES agent_registrations(id),
			desired_spec_revision INT8 NOT NULL,
			desired_rollout_generation INT8 NOT NULL,
			allocation_ipv4 STRING NOT NULL DEFAULT '',
			allocation_ipv6 STRING NOT NULL DEFAULT '',
			operator_restart_nonce INT8 NOT NULL DEFAULT 0,
			rollout_state STRING NOT NULL,
			intent STRING NOT NULL,
			intent_message STRING NOT NULL DEFAULT '',
			drain_started_at TIMESTAMPTZ NULL,
			drain_deadline TIMESTAMPTZ NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE INDEX idx_allocation_assignments_agent ON allocation_assignments(agent_id, updated_at, id)`,
	`CREATE INDEX idx_allocation_assignments_service ON allocation_assignments(service_id, id)`,
	`CREATE UNIQUE INDEX idx_allocation_assignments_owner ON allocation_assignments(id, agent_id)`,
	`CREATE UNIQUE INDEX idx_allocation_assignments_ipv4 ON allocation_assignments(allocation_ipv4) WHERE allocation_ipv4 <> ''`,
	`CREATE UNIQUE INDEX idx_allocation_assignments_ipv6 ON allocation_assignments(allocation_ipv6) WHERE allocation_ipv6 <> ''`,
	`CREATE VIEW allocations AS SELECT a.id, a.service_id, a.agent_id, a.desired_spec_revision,
			0::INT8 AS applied_spec_revision,
			a.desired_rollout_generation,
			0::INT8 AS applied_rollout_generation,
			CASE WHEN a.rollout_state = 'lost' THEN 'Unavailable'
			     WHEN a.rollout_state = 'withdrawing' THEN 'Withdrawing'
			     WHEN a.rollout_state = 'draining' THEN 'Draining'
			     ELSE 'Pending' END AS phase,
			a.intent_message AS message,
			a.allocation_ipv4, a.allocation_ipv6,
			'[]'::JSONB AS healthy_ipv4_ports,
			'[]'::JSONB AS healthy_ipv6_ports,
			FALSE AS healthy,
			'{}'::JSONB AS restart_observation_json,
			a.operator_restart_nonce, a.rollout_state, a.drain_started_at, a.drain_deadline,
			a.created_at, a.updated_at,
			a.deployment_id, a.intent
		FROM allocation_assignments a`,
	`CREATE TABLE service_rollouts (
			service_id STRING NOT NULL REFERENCES services(id) ON DELETE CASCADE,
			rollout_generation INT8 NOT NULL,
			spec_revision INT8 NOT NULL,
			reason STRING NOT NULL,
			build_id STRING NULL,
			requested_by_user_id STRING NOT NULL DEFAULT '',
			state STRING NOT NULL,
			strategy_json JSONB NOT NULL,
			desired_replica_count INT8 NOT NULL,
			image_digest STRING NOT NULL DEFAULT '',
			failure_reason STRING NOT NULL DEFAULT '',
			target_allocation_id STRING NULL,
			completed_at TIMESTAMPTZ NULL,
			progress_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (service_id, rollout_generation),
			FOREIGN KEY (service_id, spec_revision)
				REFERENCES service_revisions(service_id, spec_revision)
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
			resolved_spec_json JSONB NOT NULL,
			variable_versions_json JSONB NOT NULL,
			sealed_versions_json JSONB NOT NULL,
			is_current BOOL NOT NULL DEFAULT FALSE,
			requested_by_user_id STRING NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			UNIQUE (id, service_id),
			FOREIGN KEY (service_id, spec_revision)
				REFERENCES service_revisions(service_id, spec_revision)
		)`,
	`CREATE UNIQUE INDEX idx_deployments_service_current
			ON deployments(service_id) WHERE is_current = TRUE`,
	`CREATE INDEX idx_deployments_service_build
			ON deployments(service_id, build_id) WHERE build_id != ''`,
	`CREATE INDEX idx_deployments_service_rollout
			ON deployments(service_id, rollout_generation DESC, created_at DESC, id)`,
	`CREATE INDEX idx_deployments_service_updated
			ON deployments(service_id, updated_at DESC, id)`,
	`ALTER TABLE allocation_assignments ADD CONSTRAINT fk_allocation_assignment_deployment
			FOREIGN KEY (deployment_id, service_id) REFERENCES deployments(id, service_id)`,
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
	`CREATE TABLE deployment_actions (
			id STRING PRIMARY KEY,
			service_id STRING NOT NULL REFERENCES services(id) ON DELETE CASCADE,
			target_deployment_id STRING NOT NULL,
			result_deployment_id STRING NULL,
			action STRING NOT NULL,
			allocation_id STRING NOT NULL DEFAULT '',
			idempotency_key STRING NOT NULL,
			requested_by_user_id STRING NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			UNIQUE (service_id, requested_by_user_id, idempotency_key),
			FOREIGN KEY (target_deployment_id, service_id)
				REFERENCES deployments(id, service_id) ON DELETE CASCADE,
			FOREIGN KEY (result_deployment_id, service_id)
				REFERENCES deployments(id, service_id)
		)`,
	`CREATE INDEX idx_deployment_actions_target ON deployment_actions(target_deployment_id, created_at, id)`,
	`CREATE TABLE builder_workers (
			id STRING PRIMARY KEY,
			name STRING NOT NULL,
			current_build_id STRING NOT NULL DEFAULT '',
			last_heartbeat_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			drained BOOL NOT NULL DEFAULT FALSE
		)`,
	`CREATE TABLE build_scheduler_control (
			id BOOL PRIMARY KEY,
			paused BOOL NOT NULL DEFAULT FALSE,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
	`INSERT INTO build_scheduler_control(id, paused, updated_at) VALUES (TRUE, FALSE, now())`,
	`CREATE TABLE build_runs (
			id STRING PRIMARY KEY,
			service_id STRING NOT NULL REFERENCES services(id) ON DELETE CASCADE,
			commit_sha STRING NOT NULL,
			commit_message STRING NOT NULL DEFAULT '',
			commit_author STRING NOT NULL DEFAULT '',
			state STRING NOT NULL,
			builder_id STRING NULL REFERENCES builder_workers(id) ON DELETE SET NULL,
			owner_epoch INT8 NOT NULL DEFAULT 0,
			lease_expires_at TIMESTAMPTZ NULL,
			attempt_count INT8 NOT NULL DEFAULT 0,
			attempt_limit INT8 NOT NULL DEFAULT 3,
			cancel_requested_at TIMESTAMPTZ NULL,
			cancel_requested_by STRING NOT NULL DEFAULT '',
			deadline_at TIMESTAMPTZ NULL,
			last_heartbeat_at TIMESTAMPTZ NULL,
			image_digest STRING NOT NULL DEFAULT '',
			failure_reason STRING NOT NULL DEFAULT '',
			source_revision_id STRING NULL,
			source_snapshot_id STRING NULL,
			source_snapshot_digest STRING NOT NULL DEFAULT '',
			build_recipe_json JSONB NOT NULL DEFAULT '{}',
			target_rollout_generation INT8 NOT NULL DEFAULT 0,
			queued_at TIMESTAMPTZ NOT NULL,
			started_at TIMESTAMPTZ NULL,
			finished_at TIMESTAMPTZ NULL
		)`,
	`CREATE TABLE build_attempts (
			id STRING PRIMARY KEY,
			build_id STRING NOT NULL REFERENCES build_runs(id) ON DELETE CASCADE,
			attempt_number INT8 NOT NULL,
			builder_id STRING NOT NULL,
			owner_epoch INT8 NOT NULL,
			started_at TIMESTAMPTZ NOT NULL,
			finished_at TIMESTAMPTZ NULL,
			outcome STRING NOT NULL DEFAULT 'leased',
			detail STRING NOT NULL DEFAULT '',
			UNIQUE (build_id, attempt_number)
		)`,
	`CREATE INDEX idx_build_runs_service_queued ON build_runs(service_id, queued_at DESC, id)`,
	`CREATE INDEX idx_build_runs_service_commit ON build_runs(service_id, commit_sha, queued_at DESC, id DESC)`,
	`CREATE INDEX idx_build_runs_state_queued ON build_runs(state, queued_at ASC, id)`,
	`CREATE INDEX idx_build_runs_running_lease ON build_runs(state, lease_expires_at, id) WHERE state = 'running'`,
	`CREATE INDEX idx_build_attempts_build ON build_attempts(build_id, attempt_number)`,
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
	`CREATE INDEX idx_github_webhook_deliveries_recovery
			ON github_webhook_deliveries(state, updated_at ASC, id)`,
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
			updated_at TIMESTAMPTZ NOT NULL,
			UNIQUE (id, service_id)
		)`,
	`CREATE INDEX idx_source_bindings_provider_repo_ref
			ON source_bindings(provider, provider_repository_external_id, tracked_ref, service_id)`,
	`CREATE INDEX idx_source_bindings_provider_scope
			ON source_bindings(provider, provider_scope_external_id, service_id)`,
	`CREATE TABLE source_revisions (
			id STRING PRIMARY KEY,
			source_binding_id STRING NOT NULL,
			service_id STRING NOT NULL REFERENCES services(id) ON DELETE CASCADE,
			provider STRING NOT NULL,
			provider_repository_external_id STRING NOT NULL DEFAULT '',
			tracked_ref STRING NOT NULL DEFAULT '',
			commit_sha STRING NOT NULL,
			commit_message STRING NOT NULL DEFAULT '',
			commit_author STRING NOT NULL DEFAULT '',
			observed_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			UNIQUE (source_binding_id, commit_sha),
			FOREIGN KEY (source_binding_id, service_id)
				REFERENCES source_bindings(id, service_id) ON DELETE CASCADE
		)`,
	`CREATE INDEX idx_source_revisions_service_observed ON source_revisions(service_id, observed_at DESC, id)`,
	`CREATE INDEX idx_source_revisions_provider_repo_commit
			ON source_revisions(provider, provider_repository_external_id, commit_sha, id)`,
	`CREATE TABLE source_snapshots (
			id STRING PRIMARY KEY,
			source_revision_id STRING NULL UNIQUE REFERENCES source_revisions(id) ON DELETE SET NULL,
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
	`CREATE TABLE source_archive_objects (
			object_key STRING PRIMARY KEY,
			digest STRING NOT NULL UNIQUE,
			size_bytes INT8 NOT NULL CHECK (size_bytes > 0),
			state STRING NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE INDEX idx_source_archive_objects_gc ON source_archive_objects(state, updated_at, object_key)`,
	`CREATE TABLE durable_work_items (
			id STRING PRIMARY KEY,
			kind STRING NOT NULL,
			dedup_key STRING NOT NULL UNIQUE,
			resource_type STRING NOT NULL DEFAULT '',
			resource_id STRING NOT NULL DEFAULT '',
			state STRING NOT NULL CHECK (state IN ('pending', 'leased', 'succeeded', 'failed', 'dead')),
			attempt_count INT8 NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
			attempt_limit INT8 NOT NULL CHECK (attempt_limit > 0),
			owner_id STRING NOT NULL DEFAULT '',
			owner_epoch INT8 NOT NULL DEFAULT 0 CHECK (owner_epoch >= 0),
			lease_expires_at TIMESTAMPTZ NULL,
			last_error STRING NOT NULL DEFAULT '',
			available_at TIMESTAMPTZ NOT NULL,
			payload JSONB NOT NULL DEFAULT '{}',
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			completed_at TIMESTAMPTZ NULL
		)`,
	`CREATE INDEX idx_durable_work_items_claim ON durable_work_items(state, available_at ASC, created_at ASC, id)`,
	`CREATE INDEX idx_durable_work_items_kind ON durable_work_items(kind, state, available_at ASC, id)`,
	`CREATE INDEX idx_durable_work_items_resource ON durable_work_items(resource_type, resource_id, state)`,
	`ALTER TABLE service_delivery_status ADD CONSTRAINT fk_service_delivery_status_latest_build
			FOREIGN KEY (latest_build_id) REFERENCES build_runs(id) ON DELETE SET NULL`,
	`ALTER TABLE build_runs ADD CONSTRAINT fk_build_runs_source_revision
			FOREIGN KEY (source_revision_id) REFERENCES source_revisions(id) ON DELETE SET NULL`,
	`ALTER TABLE build_runs ADD CONSTRAINT fk_build_runs_source_snapshot
			FOREIGN KEY (source_snapshot_id) REFERENCES source_snapshots(id) ON DELETE SET NULL`,
	`ALTER TABLE service_rollouts ADD CONSTRAINT fk_service_rollouts_build
			FOREIGN KEY (build_id) REFERENCES build_runs(id) ON DELETE SET NULL`,
	`ALTER TABLE service_rollouts ADD CONSTRAINT fk_service_rollouts_target_allocation
			FOREIGN KEY (target_allocation_id) REFERENCES allocation_assignments(id) ON DELETE SET NULL`,
	`CREATE TABLE envelope_keys (
			id STRING PRIMARY KEY,
			provider STRING NOT NULL,
			provider_ref STRING NOT NULL,
			state STRING NOT NULL CHECK (state IN ('active', 'retired')),
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE UNIQUE INDEX idx_envelope_keys_single_active
			ON envelope_keys(state) WHERE state = 'active'`,
	`CREATE TABLE envelope_data_keys (
			id STRING PRIMARY KEY,
			scope_kind STRING NOT NULL,
			scope_id STRING NOT NULL,
			wrapping_key_id STRING NOT NULL REFERENCES envelope_keys(id),
			wrapped_dek BYTES NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			UNIQUE (scope_kind, scope_id)
		)`,
	`CREATE INDEX idx_envelope_data_keys_wrapping ON envelope_data_keys(wrapping_key_id)`,
	`CREATE TABLE service_secret_versions (
			service_id STRING NOT NULL REFERENCES services(id) ON DELETE CASCADE,
			name STRING NOT NULL CHECK (btrim(name) <> ''),
			version INT8 NOT NULL CHECK (version > 0),
			environment_id STRING NOT NULL,
			dek_id STRING NOT NULL REFERENCES envelope_data_keys(id),
			nonce BYTES NOT NULL,
			ciphertext BYTES NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (service_id, name, version)
		)`,
	`CREATE INDEX idx_service_secret_versions_service
			ON service_secret_versions(service_id, name, version DESC)`,
	`CREATE TABLE service_secret_tombstones (
			service_id STRING NOT NULL REFERENCES services(id) ON DELETE CASCADE,
			name STRING NOT NULL CHECK (btrim(name) <> ''),
			deleted_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (service_id, name)
		)`,
	`CREATE TABLE platform_signing_keys (
			id STRING PRIMARY KEY,
			scope STRING NOT NULL CHECK (scope IN ('internal-ca', 'registry', 'user-assertion', 'dashboard-session')),
			kid STRING NOT NULL UNIQUE,
			state STRING NOT NULL CHECK (state IN ('active', 'retiring')),
			key_type STRING NOT NULL CHECK (key_type IN ('ecdsa-p256', 'hmac-256')),
			wrapping_key_id STRING NOT NULL REFERENCES envelope_keys(id),
			wrapped_key BYTES NOT NULL,
			public_pem STRING NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			retired_at TIMESTAMPTZ NULL
		)`,
	`CREATE UNIQUE INDEX idx_platform_signing_keys_scope_state
			ON platform_signing_keys(scope, state)`,
	`CREATE INDEX idx_platform_signing_keys_wrapping ON platform_signing_keys(wrapping_key_id)`,
	`CREATE TABLE xds_publications (
			id BOOL PRIMARY KEY,
			version STRING NOT NULL,
			hash STRING NOT NULL,
			inputs BYTES NOT NULL DEFAULT '',
			listeners INT8 NOT NULL DEFAULT 0,
			clusters INT8 NOT NULL DEFAULT 0,
			endpoints INT8 NOT NULL DEFAULT 0,
			domains INT8 NOT NULL DEFAULT 0,
			publisher STRING NOT NULL DEFAULT '',
			updated_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE TABLE xds_node_observations (
			node_id STRING PRIMARY KEY,
			applied_hash STRING NOT NULL DEFAULT '',
			nacks INT8 NOT NULL DEFAULT 0,
			last_nack STRING NOT NULL DEFAULT '',
			updated_at TIMESTAMPTZ NOT NULL
		)`,
}
