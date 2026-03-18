.PHONY: test-unit-go test-unit-dashboard test-integration test-e2e-local test-e2e-vm dev-ephemeral

test-unit-go:
	go test ./...

test-unit-dashboard:
	pnpm --dir dashboard test:unit

test-integration:
	./scripts/go-test-integration.zsh
	pnpm --dir dashboard test:integration

test-e2e-local:
	go run ./cmd/localteststack

dev-ephemeral:
	LOCALTESTSTACK_RUN_PLAYWRIGHT=0 go run ./cmd/localteststack

test-e2e-vm:
	go run ./cmd/testvm
