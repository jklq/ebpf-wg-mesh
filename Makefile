.PHONY: test-unit-go test-unit-console test-integration test-e2e-local test-e2e-vm dev-ephemeral

test-unit-go:
	go test ./...

test-unit-console:
	bun --cwd=console run test:unit

test-integration:
	./scripts/go-test-integration.zsh
	bun --cwd=console run test:integration

test-e2e-local:
	bun --cwd=console run test:e2e:local

dev-ephemeral:
	LOCALTESTSTACK_RUN_PLAYWRIGHT=0 go run ./cmd/localteststack

test-e2e-vm:
	go run ./cmd/testvm
