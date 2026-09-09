.PHONY: test-unit-go test-unit-console test-integration test-integration-go test-integration-console test-linux-runtime test-linux-runtime-docker test-e2e-local test-e2e-vm dev-ephemeral

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
