package controlplane

const currentSchemaVersion = 41

// currentSchema contains both owned relational references and retained external
// identifiers. User IDs, GitHub repository links, and certificate enrollment
// records intentionally have no foreign key: their owning system or lifecycle
// is outside the referenced control-plane row.
var currentSchema = []string{
	`CREATE TABLE cluster_journal_heads (
		cluster_id TEXT PRIMARY KEY,
		log_index INT8 NOT NULL CHECK (log_index >= 0),
		compacted_index INT8 NOT NULL DEFAULT 0 CHECK (compacted_index >= 0 AND compacted_index <= log_index)
	)`,
	`INSERT INTO cluster_journal_heads(cluster_id, log_index, compacted_index) VALUES ('default', 0, 0)`,
	`CREATE TABLE cluster_journal (
		cluster_id TEXT NOT NULL REFERENCES cluster_journal_heads(cluster_id),
		log_index INT8 NOT NULL CHECK (log_index > 0),
		command_id TEXT NOT NULL,
		command_version INT8 NOT NULL,
		command_type TEXT NOT NULL,
		payload JSONB NOT NULL,
		authorizing_epoch INT8 NULL CHECK (authorizing_epoch > 0),
		created_at TIMESTAMPTZ NOT NULL,
		PRIMARY KEY (cluster_id, log_index),
		UNIQUE (cluster_id, command_id)
	)`,
	`CREATE TABLE cluster_journal_receipts (
		cluster_id TEXT NOT NULL REFERENCES cluster_journal_heads(cluster_id),
		command_id TEXT NOT NULL,
		log_index INT8 NOT NULL CHECK (log_index >= 0),
		command_version INT8 NOT NULL,
		command_type TEXT NOT NULL,
		payload JSONB NOT NULL,
		authorizing_epoch INT8 NULL CHECK (authorizing_epoch > 0),
		created_at TIMESTAMPTZ NOT NULL,
		expires_at TIMESTAMPTZ NOT NULL,
		PRIMARY KEY (cluster_id, command_id)
	)`,
	`CREATE INDEX idx_cluster_journal_receipts_expiry ON cluster_journal_receipts(cluster_id, expires_at)`,
	`CREATE TABLE control_plane_leases (
			name TEXT PRIMARY KEY,
			holder_id TEXT NOT NULL,
			fencing_token INT8 NOT NULL,
			advertise_addr TEXT NOT NULL DEFAULT '',
			expires_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE TABLE environment_events (
			environment_id TEXT PRIMARY KEY,
			revision INT8 NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE TABLE control_plane_storage (
			name TEXT PRIMARY KEY,
			storage_id TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE TABLE projects (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			kind TEXT NOT NULL,
			system_key TEXT NULL,
			owner_user_id TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			deleted_at TIMESTAMPTZ NULL,
			deleted_by_user_id TEXT NOT NULL DEFAULT '',
			delete_expires_at TIMESTAMPTZ NULL,
			log_retention_days INT NOT NULL DEFAULT 0
		)`,
	`CREATE INDEX idx_projects_delete_expires ON projects(delete_expires_at, id) WHERE deleted_at IS NOT NULL`,
	`CREATE UNIQUE INDEX idx_projects_owner_name
			ON projects(owner_user_id, name) WHERE kind = 'user'`,
	`CREATE UNIQUE INDEX idx_projects_system_key
			ON projects(system_key) WHERE system_key IS NOT NULL`,
	`CREATE TABLE project_memberships (
			user_id TEXT NOT NULL,
			project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			role TEXT NOT NULL,
			PRIMARY KEY (user_id, project_id)
		)`,
	`CREATE INDEX idx_project_memberships_user ON project_memberships(user_id, project_id)`,
	`CREATE TABLE platform_operators (
			user_id TEXT PRIMARY KEY,
			created_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE TABLE environment_network_identity_counter (
			id BOOL PRIMARY KEY,
			next_identity INT8 NOT NULL
		)`,
	`INSERT INTO environment_network_identity_counter(id, next_identity) VALUES (TRUE, 1)`,
	`CREATE TABLE workload_ipv4_prefix_allocator (
			id BOOL PRIMARY KEY,
			pool_cidr TEXT NOT NULL,
			prefix_bits INT8 NOT NULL,
			next_ordinal INT8 NOT NULL
		)`,
	`CREATE TABLE environments (
			id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			kind TEXT NOT NULL,
			is_production BOOL NOT NULL DEFAULT FALSE,
			auto_deploy BOOL NOT NULL,
			network_identity INT8 NOT NULL UNIQUE,
			copied_from_environment_id TEXT NULL REFERENCES environments(id) ON DELETE SET NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			deleted_at TIMESTAMPTZ NULL,
			deleted_by_user_id TEXT NOT NULL DEFAULT '',
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
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
 local_store_id TEXT NOT NULL DEFAULT '',
 session_incarnation INT8 NOT NULL DEFAULT 0,
			region TEXT NOT NULL,
			zone TEXT NOT NULL DEFAULT '',
			failure_domain TEXT NOT NULL,
			reserved_cpu_millis INT8 NOT NULL DEFAULT 0,
			reserved_memory_mebibytes INT8 NOT NULL DEFAULT 0,
			advertise_addr TEXT NOT NULL DEFAULT '',
			workload_ipv4_subnet TEXT NOT NULL DEFAULT '',
			workload_ipv6_subnet TEXT NOT NULL DEFAULT '',
			wireguard_public_key TEXT NOT NULL DEFAULT '',
			wireguard_listen_port INT8 NOT NULL DEFAULT 0,
			wireguard_endpoint TEXT NOT NULL DEFAULT '',
			wireguard_ipv6 TEXT NOT NULL DEFAULT '',
			cpu_millis_capacity INT8 NOT NULL DEFAULT 0,
			memory_mebibytes_capacity INT8 NOT NULL DEFAULT 0,
			runtime_capabilities JSONB NOT NULL DEFAULT '[]',
			software_version TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			desired_revision INT8 NOT NULL DEFAULT 0
		)`,
	`CREATE UNIQUE INDEX idx_agent_registrations_workload_ipv4_subnet ON agent_registrations(workload_ipv4_subnet) WHERE workload_ipv4_subnet <> ''`,
	`CREATE TABLE agent_administration (
			agent_id TEXT PRIMARY KEY REFERENCES agent_registrations(id) ON DELETE CASCADE,
 host_type TEXT NOT NULL DEFAULT 'stable' CHECK (host_type IN ('stable', 'intermittent')),
			lifecycle_state TEXT NOT NULL,
			operator_intent TEXT NOT NULL DEFAULT '',
			maintenance_message TEXT NOT NULL DEFAULT '',
			credential_revoked_at TIMESTAMPTZ NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE VIEW agents AS SELECT r.id, r.name,
			ad.host_type, ad.lifecycle_state,
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
			token_hash BYTEA PRIMARY KEY,
			agent_id TEXT NOT NULL,
			origin TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			consumed_at TIMESTAMPTZ NULL,
			csr_public_key_sha256 BYTEA NULL
		)`,
	`CREATE INDEX idx_agent_bootstrap_tokens_agent ON agent_bootstrap_tokens(agent_id, consumed_at)`,
	`CREATE TABLE agent_certificates (
			serial TEXT PRIMARY KEY,
			agent_id TEXT NOT NULL,
			issued_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE INDEX idx_agent_certificates_agent ON agent_certificates(agent_id, issued_at DESC)`,
	`CREATE TABLE volumes (
			id TEXT PRIMARY KEY,
			environment_id TEXT NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
			name TEXT NOT NULL CHECK (btrim(name) <> ''),
			size_bytes INT8 NOT NULL CHECK (size_bytes > 0),
			agent_id TEXT NULL REFERENCES agent_registrations(id),
			staged BOOL NOT NULL DEFAULT false,
			created_at TIMESTAMPTZ NOT NULL,
			deleted_at TIMESTAMPTZ NULL,
			deleted_by_user_id TEXT NOT NULL DEFAULT '',
			delete_expires_at TIMESTAMPTZ NULL,
			UNIQUE (environment_id, name)
		)`,
	`CREATE INDEX idx_volumes_delete_expires ON volumes(delete_expires_at, id) WHERE deleted_at IS NOT NULL`,
	`CREATE INDEX idx_volumes_environment_created ON volumes(environment_id, created_at, id)`,
	`CREATE INDEX idx_volumes_agent ON volumes(agent_id) WHERE agent_id IS NOT NULL`,
	// A destruction outlives its volume row (and the environment that owned
	// it): the pinned agent deletes data only on this explicit instruction,
	// and the row is removed once the agent reports the data gone.
	`CREATE TABLE volume_destructions (
			volume_id TEXT PRIMARY KEY,
			agent_id TEXT NOT NULL REFERENCES agent_registrations(id),
			requested_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE INDEX idx_volume_destructions_agent ON volume_destructions(agent_id)`,
	`CREATE TABLE envelope_keys (
			id TEXT PRIMARY KEY,
			provider TEXT NOT NULL,
			provider_ref TEXT NOT NULL,
			state TEXT NOT NULL CHECK (state IN ('active', 'retired')),
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE UNIQUE INDEX idx_envelope_keys_single_active
			ON envelope_keys(state) WHERE state = 'active'`,
	`CREATE TABLE envelope_data_keys (
			id TEXT PRIMARY KEY,
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			wrapping_key_id TEXT NOT NULL REFERENCES envelope_keys(id),
			wrapped_dek BYTEA NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			UNIQUE (scope_kind, scope_id)
		)`,
	`CREATE INDEX idx_envelope_data_keys_wrapping ON envelope_data_keys(wrapping_key_id)`,
	`CREATE TABLE services (
			id TEXT PRIMARY KEY,
			environment_id TEXT NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			current_spec_revision INT8 NOT NULL,
			desired_replica_count INT8 NOT NULL DEFAULT 1,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			deleted_at TIMESTAMPTZ NULL,
			deleted_by_user_id TEXT NOT NULL DEFAULT '',
			delete_expires_at TIMESTAMPTZ NULL,
			UNIQUE (environment_id, name)
		)`,
	`CREATE INDEX idx_services_delete_expires ON services(delete_expires_at, id) WHERE deleted_at IS NOT NULL`,
	`CREATE INDEX idx_services_environment_created ON services(environment_id, created_at, id)`,
	`CREATE VIEW live_services AS
		 SELECT s.* FROM services s
		 JOIN environments e ON e.id = s.environment_id
		 JOIN projects p ON p.id = e.project_id
		 WHERE s.deleted_at IS NULL AND e.deleted_at IS NULL AND p.deleted_at IS NULL`,
	// spec_json never holds runtime env: the env map is encrypted under the
	// environment's DEK into env_ciphertext, bound to (service_id, spec_revision).
	`CREATE TABLE service_revisions (
			service_id TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
			spec_revision INT8 NOT NULL,
			spec_json JSONB NOT NULL,
			env_dek_id TEXT NULL REFERENCES envelope_data_keys(id),
			env_ciphertext BYTEA NULL,
			created_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (service_id, spec_revision),
			CHECK ((env_dek_id IS NULL) = (env_ciphertext IS NULL))
	)`,
	`CREATE TABLE service_delivery_status (
			service_id TEXT PRIMARY KEY REFERENCES services(id) ON DELETE CASCADE,
			current_rollout_generation INT8 NULL,
			current_artifact_id TEXT NULL,
			last_successful_commit_sha TEXT NULL,
			latest_build_id TEXT NULL,
			placement_message TEXT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE TABLE domain_bindings (
			hostname TEXT PRIMARY KEY,
			service_id TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
			target_port INT8 NOT NULL DEFAULT 8080,
			platform_generated BOOL NOT NULL DEFAULT FALSE,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			deleted_at TIMESTAMPTZ NULL,
			deleted_by_user_id TEXT NOT NULL DEFAULT '',
			delete_expires_at TIMESTAMPTZ NULL
		)`,
	`CREATE INDEX idx_domain_bindings_delete_expires ON domain_bindings(delete_expires_at, hostname) WHERE deleted_at IS NOT NULL`,
	`CREATE INDEX idx_domain_bindings_service ON domain_bindings(service_id, hostname)`,
	`CREATE UNIQUE INDEX idx_domain_bindings_generated_service
			ON domain_bindings(service_id) WHERE platform_generated = TRUE`,
	`CREATE TABLE allocation_assignments (
			id TEXT PRIMARY KEY,
			service_id TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
			deployment_id TEXT NOT NULL,
			agent_id TEXT NOT NULL REFERENCES agent_registrations(id),
			desired_spec_revision INT8 NOT NULL,
			desired_rollout_generation INT8 NOT NULL,
			allocation_ipv4 TEXT NOT NULL DEFAULT '',
			allocation_ipv6 TEXT NOT NULL DEFAULT '',
			operator_restart_nonce INT8 NOT NULL DEFAULT 0,
			rollout_state TEXT NOT NULL,
			intent TEXT NOT NULL,
			intent_message TEXT NOT NULL DEFAULT '',
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
			service_id TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
			rollout_generation INT8 NOT NULL,
			spec_revision INT8 NOT NULL,
			reason TEXT NOT NULL,
			build_id TEXT NULL,
			requested_by_user_id TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL,
			strategy_json JSONB NOT NULL,
			desired_replica_count INT8 NOT NULL,
			artifact_id TEXT NULL,
			failure_reason TEXT NOT NULL DEFAULT '',
			target_allocation_id TEXT NULL,
			recreate BOOL NOT NULL DEFAULT false,
			completed_at TIMESTAMPTZ NULL,
			progress_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (service_id, rollout_generation),
			FOREIGN KEY (service_id, spec_revision)
				REFERENCES service_revisions(service_id, spec_revision)
		)`,
	`CREATE INDEX idx_service_rollouts_in_progress ON service_rollouts(state, created_at, service_id)`,
	`CREATE TABLE deployments (
			id TEXT PRIMARY KEY,
			service_id TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
			spec_revision INT8 NOT NULL,
			rollout_generation INT8 NOT NULL DEFAULT 0,
			build_id TEXT NOT NULL DEFAULT '',
			artifact_id TEXT NULL,
			source_revision_id TEXT NULL,
			state TEXT NOT NULL,
			cause_kind TEXT NOT NULL,
			cause_id TEXT NOT NULL DEFAULT '',
			reason_code TEXT NOT NULL,
			detail TEXT NOT NULL DEFAULT '',
			is_current BOOL NOT NULL DEFAULT FALSE,
			requested_by_user_id TEXT NOT NULL DEFAULT '',
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
			id TEXT PRIMARY KEY,
			deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
			from_state TEXT NOT NULL,
			to_state TEXT NOT NULL,
			cause_kind TEXT NOT NULL,
			cause_id TEXT NOT NULL DEFAULT '',
			reason_code TEXT NOT NULL,
			detail TEXT NOT NULL DEFAULT '',
			spec_revision INT8 NOT NULL,
			artifact_id TEXT NULL,
			rollout_generation INT8 NOT NULL DEFAULT 0,
			occurred_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE INDEX idx_deployment_transitions_deployment
			ON deployment_transitions(deployment_id, occurred_at ASC, id)`,
	`CREATE TABLE deployment_actions (
			id TEXT PRIMARY KEY,
			service_id TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
			target_deployment_id TEXT NOT NULL,
			result_deployment_id TEXT NULL,
			action TEXT NOT NULL,
			allocation_id TEXT NOT NULL DEFAULT '',
			idempotency_key TEXT NOT NULL,
			requested_by_user_id TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			UNIQUE (service_id, requested_by_user_id, idempotency_key),
			FOREIGN KEY (target_deployment_id, service_id)
				REFERENCES deployments(id, service_id) ON DELETE CASCADE,
			FOREIGN KEY (result_deployment_id, service_id)
				REFERENCES deployments(id, service_id)
		)`,
	`CREATE INDEX idx_deployment_actions_target ON deployment_actions(target_deployment_id, created_at, id)`,
	`CREATE TABLE builder_workers (
 host_type TEXT NOT NULL DEFAULT 'stable' CHECK (host_type IN ('stable', 'intermittent')),
 available_memory_bytes INT8 NOT NULL DEFAULT 0,
 required_memory_bytes INT8 NOT NULL DEFAULT 0,
 available_cpu_millis INT8 NOT NULL DEFAULT 0,
 required_cpu_millis INT8 NOT NULL DEFAULT 0,
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			current_build_id TEXT NOT NULL DEFAULT '',
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
			id TEXT PRIMARY KEY,
			service_id TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
			commit_sha TEXT NOT NULL,
			commit_message TEXT NOT NULL DEFAULT '',
			commit_contributors JSONB NOT NULL DEFAULT '[]',
			state TEXT NOT NULL,
			builder_id TEXT NULL REFERENCES builder_workers(id) ON DELETE SET NULL,
			owner_epoch INT8 NOT NULL DEFAULT 0,
			lease_expires_at TIMESTAMPTZ NULL,
			attempt_count INT8 NOT NULL DEFAULT 0,
			attempt_limit INT8 NOT NULL DEFAULT 3,
			cancel_requested_at TIMESTAMPTZ NULL,
			cancel_requested_by TEXT NOT NULL DEFAULT '',
			deadline_at TIMESTAMPTZ NULL,
			last_heartbeat_at TIMESTAMPTZ NULL,
			artifact_id TEXT NULL,
			failure_reason TEXT NOT NULL DEFAULT '',
			source_revision_id TEXT NULL,
			source_snapshot_id TEXT NULL,
			source_snapshot_digest TEXT NOT NULL DEFAULT '',
			build_recipe_json JSONB NOT NULL DEFAULT '{}',
			build_actor_kind TEXT NOT NULL DEFAULT '',
			build_actor_id TEXT NOT NULL DEFAULT '',
			target_rollout_generation INT8 NOT NULL DEFAULT 0,
			queued_at TIMESTAMPTZ NOT NULL,
			started_at TIMESTAMPTZ NULL,
			finished_at TIMESTAMPTZ NULL
		)`,
	`CREATE TABLE build_attempts (
			id TEXT PRIMARY KEY,
			build_id TEXT NOT NULL REFERENCES build_runs(id) ON DELETE CASCADE,
			attempt_number INT8 NOT NULL,
			builder_id TEXT NOT NULL,
			owner_epoch INT8 NOT NULL,
			started_at TIMESTAMPTZ NOT NULL,
			finished_at TIMESTAMPTZ NULL,
			outcome TEXT NOT NULL DEFAULT 'leased',
			detail TEXT NOT NULL DEFAULT '',
			UNIQUE (build_id, attempt_number)
		)`,
	`CREATE INDEX idx_build_runs_service_queued ON build_runs(service_id, queued_at DESC, id)`,
	`CREATE INDEX idx_build_runs_service_commit ON build_runs(service_id, commit_sha, queued_at DESC, id DESC)`,
	`CREATE INDEX idx_build_runs_state_queued ON build_runs(state, queued_at ASC, id)`,
	`CREATE INDEX idx_build_runs_running_lease ON build_runs(state, lease_expires_at, id) WHERE state = 'running'`,
	`CREATE INDEX idx_build_attempts_build ON build_attempts(build_id, attempt_number)`,
	`CREATE TABLE build_artifacts (
			id TEXT PRIMARY KEY,
			service_id TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
			build_id TEXT NULL REFERENCES build_runs(id) ON DELETE SET NULL,
			kind TEXT NOT NULL CHECK (kind IN ('build', 'direct_image')),
			source_snapshot_digest TEXT NOT NULL DEFAULT '',
			commit_sha TEXT NOT NULL DEFAULT '',
			build_recipe_json JSONB NOT NULL DEFAULT '{}',
			builder_version TEXT NOT NULL DEFAULT '',
			image_repository TEXT NOT NULL,
			image_manifest_digest TEXT NOT NULL CHECK (image_manifest_digest LIKE 'sha256:%'),
			image_ref TEXT NOT NULL,
			source_image_ref TEXT NOT NULL DEFAULT '',
			image_retained BOOL NOT NULL DEFAULT TRUE,
			reuse_key TEXT NOT NULL DEFAULT '',
			build_actor_kind TEXT NOT NULL DEFAULT '',
			build_actor_id TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE INDEX idx_build_artifacts_service_reuse ON build_artifacts(service_id, reuse_key) WHERE reuse_key <> ''`,
	`CREATE UNIQUE INDEX idx_build_artifacts_service_direct_ref ON build_artifacts(service_id, image_ref, source_image_ref) WHERE kind = 'direct_image'`,
	`CREATE INDEX idx_build_artifacts_service_created ON build_artifacts(service_id, created_at DESC, id)`,
	`CREATE UNIQUE INDEX idx_build_artifacts_build ON build_artifacts(build_id) WHERE build_id IS NOT NULL`,
	`CREATE TABLE registry_image_deletions (
		image_ref TEXT PRIMARY KEY,
		created_at TIMESTAMPTZ NOT NULL
	)`,
	`CREATE TABLE github_installations (
			installation_id INT8 PRIMARY KEY,
			account_login TEXT NOT NULL,
			account_type TEXT NOT NULL,
			target_type TEXT NOT NULL,
			active BOOL NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE TABLE github_installation_repositories (
			installation_id INT8 NOT NULL REFERENCES github_installations(installation_id) ON DELETE CASCADE,
			repository_id INT8 NOT NULL,
			owner TEXT NOT NULL,
			repo TEXT NOT NULL,
			full_name TEXT NOT NULL,
			private BOOL NOT NULL,
			default_branch TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (installation_id, repository_id),
			UNIQUE (installation_id, full_name)
		)`,
	`CREATE INDEX idx_github_installation_repositories_name
			ON github_installation_repositories(full_name, installation_id)`,
	`CREATE TABLE project_github_repositories (
			project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			installation_id INT8 NOT NULL,
			repository_id INT8 NOT NULL,
			full_name TEXT NOT NULL,
			linked_by_user_id TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (project_id, repository_id),
			UNIQUE (project_id, full_name)
		)`,
	`CREATE INDEX idx_project_github_repositories_installation
			ON project_github_repositories(installation_id, repository_id, project_id)`,
	`CREATE TABLE github_repository_snapshots (
			full_name TEXT PRIMARY KEY,
			repository_id INT8 NOT NULL DEFAULT 0,
			owner TEXT NOT NULL,
			repo TEXT NOT NULL,
			private BOOL NOT NULL DEFAULT FALSE,
			default_branch TEXT NOT NULL DEFAULT '',
			deleted BOOL NOT NULL DEFAULT FALSE,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE INDEX idx_github_repository_snapshots_owner_repo
			ON github_repository_snapshots(owner, repo)`,
	`CREATE TABLE github_webhook_deliveries (
			id TEXT PRIMARY KEY,
			delivery_id TEXT NOT NULL UNIQUE,
			event_type TEXT NOT NULL,
			state TEXT NOT NULL,
			processor_id TEXT NOT NULL DEFAULT '',
			payload JSONB NOT NULL,
			last_error TEXT NOT NULL DEFAULT '',
			received_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			processed_at TIMESTAMPTZ NULL
		)`,
	`CREATE INDEX idx_github_webhook_deliveries_state
			ON github_webhook_deliveries(state, received_at ASC, id)`,
	`CREATE INDEX idx_github_webhook_deliveries_recovery
			ON github_webhook_deliveries(state, updated_at ASC, id)`,
	`CREATE TABLE github_work_items (
			id TEXT PRIMARY KEY,
			kind TEXT NOT NULL,
			state TEXT NOT NULL,
			processor_id TEXT NOT NULL DEFAULT '',
			idempotency_key TEXT NOT NULL UNIQUE,
			service_id TEXT NOT NULL DEFAULT '',
			spec_revision INT8 NOT NULL DEFAULT 0,
			installation_id INT8 NOT NULL DEFAULT 0,
			commit_sha TEXT NOT NULL DEFAULT '',
			last_error TEXT NOT NULL DEFAULT '',
			attempt_count INT8 NOT NULL DEFAULT 0,
			available_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE INDEX idx_github_work_items_state ON github_work_items(state, available_at ASC, created_at ASC, id)`,
	`CREATE TABLE source_bindings (
			id TEXT PRIMARY KEY,
			service_id TEXT NOT NULL UNIQUE REFERENCES services(id) ON DELETE CASCADE,
			provider TEXT NOT NULL,
			repository_selector TEXT NOT NULL DEFAULT '',
			tracked_ref TEXT NOT NULL DEFAULT '',
			provider_repository_external_id TEXT NOT NULL DEFAULT '',
			provider_scope_external_id TEXT NOT NULL DEFAULT '',
			access_state TEXT NOT NULL DEFAULT '',
			build_recipe_json JSONB NOT NULL DEFAULT '{}',
			head_commit_sha TEXT NOT NULL DEFAULT '',
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
			id TEXT PRIMARY KEY,
			source_binding_id TEXT NOT NULL,
			service_id TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
			provider TEXT NOT NULL,
			provider_repository_external_id TEXT NOT NULL DEFAULT '',
			tracked_ref TEXT NOT NULL DEFAULT '',
			commit_sha TEXT NOT NULL,
			commit_message TEXT NOT NULL DEFAULT '',
			commit_contributors JSONB NOT NULL DEFAULT '[]',
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
			id TEXT PRIMARY KEY,
			source_revision_id TEXT NULL UNIQUE REFERENCES source_revisions(id) ON DELETE SET NULL,
			provider TEXT NOT NULL,
			provider_repository_external_id TEXT NOT NULL DEFAULT '',
			commit_sha TEXT NOT NULL,
			digest TEXT NOT NULL DEFAULT '',
			object_key TEXT NOT NULL DEFAULT '',
			archive_size_bytes INT8 NOT NULL DEFAULT 0,
			ready BOOL NOT NULL DEFAULT FALSE,
			fetched_at TIMESTAMPTZ NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			UNIQUE (provider, provider_repository_external_id, commit_sha)
		)`,
	`CREATE INDEX idx_source_snapshots_created ON source_snapshots(created_at, id)`,
	`CREATE TABLE source_archive_objects (
			object_key TEXT PRIMARY KEY,
			digest TEXT NOT NULL UNIQUE,
			size_bytes INT8 NOT NULL CHECK (size_bytes > 0),
			state TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE INDEX idx_source_archive_objects_gc ON source_archive_objects(state, updated_at, object_key)`,
	`CREATE TABLE durable_work_items (
			id TEXT PRIMARY KEY,
			kind TEXT NOT NULL,
			dedup_key TEXT NOT NULL UNIQUE,
			resource_type TEXT NOT NULL DEFAULT '',
			resource_id TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL CHECK (state IN ('pending', 'leased', 'succeeded', 'failed', 'dead')),
			attempt_count INT8 NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
			attempt_limit INT8 NOT NULL CHECK (attempt_limit > 0),
			owner_id TEXT NOT NULL DEFAULT '',
			owner_epoch INT8 NOT NULL DEFAULT 0 CHECK (owner_epoch >= 0),
			lease_expires_at TIMESTAMPTZ NULL,
			last_error TEXT NOT NULL DEFAULT '',
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
	`ALTER TABLE deployments ADD CONSTRAINT fk_deployments_source_revision
		FOREIGN KEY (source_revision_id) REFERENCES source_revisions(id) ON DELETE SET NULL`,
	`ALTER TABLE build_runs ADD CONSTRAINT fk_build_runs_source_revision
			FOREIGN KEY (source_revision_id) REFERENCES source_revisions(id) ON DELETE SET NULL`,
	`ALTER TABLE build_runs ADD CONSTRAINT fk_build_runs_source_snapshot
			FOREIGN KEY (source_snapshot_id) REFERENCES source_snapshots(id) ON DELETE SET NULL`,
	`ALTER TABLE service_rollouts ADD CONSTRAINT fk_service_rollouts_build
			FOREIGN KEY (build_id) REFERENCES build_runs(id) ON DELETE SET NULL`,
	`ALTER TABLE service_rollouts ADD CONSTRAINT fk_service_rollouts_target_allocation
			FOREIGN KEY (target_allocation_id) REFERENCES allocation_assignments(id) ON DELETE SET NULL`,
	`ALTER TABLE deployments ADD CONSTRAINT fk_deployments_artifact
			FOREIGN KEY (artifact_id) REFERENCES build_artifacts(id) ON DELETE RESTRICT`,
	`ALTER TABLE deployment_transitions ADD CONSTRAINT fk_deployment_transitions_artifact
			FOREIGN KEY (artifact_id) REFERENCES build_artifacts(id) ON DELETE RESTRICT`,
	`ALTER TABLE service_rollouts ADD CONSTRAINT fk_service_rollouts_artifact
			FOREIGN KEY (artifact_id) REFERENCES build_artifacts(id) ON DELETE RESTRICT`,
	`ALTER TABLE service_delivery_status ADD CONSTRAINT fk_service_delivery_status_artifact
			FOREIGN KEY (current_artifact_id) REFERENCES build_artifacts(id) ON DELETE RESTRICT`,
	`ALTER TABLE build_runs ADD CONSTRAINT fk_build_runs_artifact
			FOREIGN KEY (artifact_id) REFERENCES build_artifacts(id) ON DELETE SET NULL`,
	`CREATE TABLE platform_signing_keys (
			id TEXT PRIMARY KEY,
			scope TEXT NOT NULL CHECK (scope IN ('internal-ca', 'registry', 'user-assertion', 'dashboard-session')),
			kid TEXT NOT NULL UNIQUE,
			state TEXT NOT NULL CHECK (state IN ('active', 'retiring')),
			key_type TEXT NOT NULL CHECK (key_type IN ('ecdsa-p256', 'hmac-256')),
			wrapping_key_id TEXT NOT NULL REFERENCES envelope_keys(id),
			wrapped_key BYTEA NOT NULL,
			public_pem TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			retired_at TIMESTAMPTZ NULL
		)`,
	`CREATE UNIQUE INDEX idx_platform_signing_keys_scope_state
			ON platform_signing_keys(scope, state)`,
	`CREATE INDEX idx_platform_signing_keys_wrapping ON platform_signing_keys(wrapping_key_id)`,
	`CREATE TABLE xds_publications (
			id BOOL PRIMARY KEY,
			version TEXT NOT NULL,
			inputs BYTEA NOT NULL DEFAULT '',
			publisher TEXT NOT NULL DEFAULT '',
			updated_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE TABLE ingress_nodes (
			node_id TEXT PRIMARY KEY,
			state TEXT NOT NULL CHECK (state IN ('active', 'retired')),
			created_at TIMESTAMPTZ NOT NULL,
			retired_at TIMESTAMPTZ NULL,
			CHECK ((state = 'retired') = (retired_at IS NOT NULL))
		)`,
	`CREATE TABLE xds_node_observations (
			node_id TEXT PRIMARY KEY REFERENCES ingress_nodes(node_id),
			applied_version TEXT NOT NULL DEFAULT '',
			nacks INT8 NOT NULL DEFAULT 0,
			last_nack TEXT NOT NULL DEFAULT '',
			updated_at TIMESTAMPTZ NOT NULL
		)`,
	// ingress_certificates holds the issuance state of each public hostname. It
	// outlives a tombstoned binding, so a restore serves HTTPS at once.
	`CREATE TABLE ingress_certificates (
			hostname TEXT PRIMARY KEY,
			fingerprint TEXT NOT NULL DEFAULT '',
			not_after TIMESTAMPTZ NULL,
			renew_at TIMESTAMPTZ NULL,
			attempts INT8 NOT NULL DEFAULT 0,
			next_attempt_at TIMESTAMPTZ NOT NULL,
			last_error TEXT NOT NULL DEFAULT '',
			updated_at TIMESTAMPTZ NOT NULL
		)`,
	// ingress_certificate_versions are immutable. A replica that adopts an older
	// publication still finds the version that it references.
	`CREATE TABLE ingress_certificate_versions (
			fingerprint TEXT PRIMARY KEY,
			hostname TEXT NOT NULL,
			chain_pem BYTEA NOT NULL,
			key_dek_id TEXT NOT NULL,
			key_ciphertext BYTEA NOT NULL,
			not_before TIMESTAMPTZ NOT NULL,
			not_after TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE INDEX idx_ingress_certificate_versions_hostname ON ingress_certificate_versions(hostname, created_at)`,
	`CREATE TABLE acme_http_challenges (
			token TEXT PRIMARY KEY,
			hostname TEXT NOT NULL,
			key_authorization TEXT NOT NULL,
			expires_at TIMESTAMPTZ NOT NULL
		)`,
	`CREATE TABLE acme_accounts (
			directory_url TEXT PRIMARY KEY,
			account_uri TEXT NOT NULL,
			key_dek_id TEXT NOT NULL,
			key_ciphertext BYTEA NOT NULL,
			created_at TIMESTAMPTZ NOT NULL
		)`,
}
