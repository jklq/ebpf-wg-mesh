.PHONY: test-unit-go test-unit-dashboard test-integration test-e2e-local test-e2e-vm dev-ephemeral

test-unit-go:
	go test ./...

test-unit-dashboard:
	bun --cwd=dashboard run test:unit

test-integration:
	./scripts/go-test-integration.zsh
	bun --cwd=dashboard run test:integration

test-e2e-local:
	bun --cwd=dashboard run test:e2e:local

dev-ephemeral:
	LOCALTESTSTACK_RUN_PLAYWRIGHT=0 go run ./cmd/localteststack

test-e2e-vm:
	go run ./cmd/testvm
