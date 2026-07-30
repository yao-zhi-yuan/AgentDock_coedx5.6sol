SHELL := /bin/sh

GO_TOOLCHAIN := go1.26.5
GOLANGCI_LINT_VERSION := v2.12.2
COMPOSE_FILE := compose.yaml
DOCTOR_ENV := .env.example
DATABASE_URL ?= postgres://agentdock:agentdock_dev_only@127.0.0.1:55433/agentdock?sslmode=disable

.DEFAULT_GOAL := help

.PHONY: help doctor compose-config \
	lint test test-race test-integration test-chaos security-test \
	migrate demo-fake demo-eino-recorded sandbox-security-test \
	chaos-worker-kill test-rebuild-state e2e-recorded e2e-replay e2e-live

help:
	@echo "AgentDock Verify — phase 2 durable event store"
	@echo "  make doctor          Validate the development environment"
	@echo "  make compose-config  Parse the pinned Docker Compose configuration"
	@echo "  make lint            Run Go static analysis"
	@echo "  make test            Run all Go tests"
	@echo "  make test-race       Run all Go tests with the race detector"
	@echo "  make migrate         Apply PostgreSQL migrations"
	@echo "  make test-integration Run PostgreSQL integration tests"
	@echo "  make test-rebuild-state Verify golden, 1000-event, and checkpoint rebuilds"
	@echo "  make demo-fake       Demonstrate a fake Run and pause/resume"
	@echo "  Later-phase targets remain intentionally unavailable"

doctor:
	@EXPECTED_GO_TOOLCHAIN="$(GO_TOOLCHAIN)" DOCTOR_ENV="$(DOCTOR_ENV)" COMPOSE_FILE="$(COMPOSE_FILE)" ./scripts/doctor.sh

compose-config:
	@docker compose --env-file "$(DOCTOR_ENV)" -f "$(COMPOSE_FILE)" config --quiet

lint:
	@go vet ./...

test:
	@go test ./...

test-race:
	@go test -race ./...

test-integration:
	@AGENTDOCK_DATABASE_URL="$(DATABASE_URL)" go test -tags=integration ./internal/store/... ./internal/controller/... ./internal/migration/... ./cmd/agentdock

test-chaos:
	@./scripts/not-implemented.sh "test-chaos" "phase 3"

security-test:
	@./scripts/not-implemented.sh "security-test" "phase 4"

migrate:
	@go run ./cmd/migrate -database-url "$(DATABASE_URL)" -path migrations

demo-fake:
	@AGENTDOCK_DATABASE_URL= go run ./cmd/agentdock demo-fake

demo-eino-recorded:
	@./scripts/not-implemented.sh "demo-eino-recorded" "phase 5"

sandbox-security-test:
	@./scripts/not-implemented.sh "sandbox-security-test" "phase 4"

chaos-worker-kill:
	@./scripts/not-implemented.sh "chaos-worker-kill" "phase 3"

test-rebuild-state:
	@go test -count=1 ./internal/domain -run 'TestReduceGoldenEventsMatchesGoldenStateFieldByField|TestReduceFromCheckpointMatchesFullReductionAcross1000Events'
	@AGENTDOCK_DATABASE_URL="$(DATABASE_URL)" go test -tags=integration -count=1 ./internal/store -run TestPostgresRebuilds1000EventsAndCheckpointMatchesFullLog

e2e-recorded:
	@./scripts/not-implemented.sh "e2e-recorded" "phase 6"

e2e-replay:
	@./scripts/not-implemented.sh "e2e-replay" "phase 7"

e2e-live:
	@./scripts/not-implemented.sh "e2e-live" "phase 5 (optional, never a CI gate)"
