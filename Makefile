.PHONY: test-unit-go test-unit-console test-integration test-integration-go test-integration-console test-linux-runtime test-linux-runtime-docker test-e2e-local test-e2e-vm dev-ephemeral localvm-setup test-stress-local plan-stress-local test-smoke-local cleanup-local test-stress-ovh plan-stress-ovh

test-unit-go:
	go test ./...

test-linux-runtime:
	go test -count=1 -timeout 10m ./internal/agent
	go test -count=1 -timeout 10m ./internal/firewall

# Same tests, but provisioned with containerd/CNI/BPF inside a privileged Linux
# container so they actually run on a non-Linux host instead of skipping.
test-linux-runtime-docker:
	./scripts/test-linux-runtime-docker.zsh

test-unit-console:
	bun --cwd=console run test:unit

test-integration:
	$(MAKE) test-integration-go
	$(MAKE) test-integration-console

test-integration-go:
	./scripts/go-test-integration.zsh

test-integration-console:
	go run ./cmd/testdb -- bun --cwd=console run test:integration

test-e2e-local:
	LOCALTESTSTACK_PRODUCT_E2E=1 \
	LOCALTESTSTACK_ENABLE_PUBLIC_TUNNEL=1 \
	LOCALTESTSTACK_CONSOLE_BIND_ADDRESS=0.0.0.0 \
	bun --cwd=console run test:e2e:local

dev-ephemeral:
	LOCALTESTSTACK_RUN_PLAYWRIGHT=0 LOCALTESTSTACK_CONSOLE_BIND_ADDRESS=0.0.0.0 go run ./cmd/localteststack

test-e2e-vm:
	go run ./cmd/testvm

# ARGS overrides defaults, e.g. ARGS='-seed 42 -agents 4 -ovh-hourly-rate 0.10'.
test-stress-ovh:
	go run ./cmd/testvm -provider ovh -scenario stress -ovh-action run $(ARGS)

plan-stress-ovh:
	go run ./cmd/testvm -provider ovh -scenario stress -stress-plan-only $(ARGS)

# Local QEMU/KVM fleet on this host. Run `make localvm-setup` once first (needs
# sudo and BIOS virtualization). ARGS overrides defaults, e.g.
# ARGS='-seed 42 -local-agents 4 -stress-stages 12'.
localvm-setup:
	sudo bash scripts/localvm-setup.sh "$${SUDO_USER:-$$USER}"

plan-stress-local:
	go run ./cmd/testvm -provider local -scenario stress -stress-plan-only $(ARGS)

test-stress-local:
	go run ./cmd/testvm -provider local -scenario stress $(ARGS)

test-smoke-local:
	go run ./cmd/testvm -provider local -scenario service-rollout $(ARGS)

cleanup-local:
	@test -n "$(MANIFEST)" || { echo "MANIFEST is required, e.g. make cleanup-local MANIFEST=artifacts/e2e-vm/<run-id>/local-resources.json"; exit 1; }
	go run ./cmd/testvm -provider local -local-action destroy -local-manifest "$(MANIFEST)"
