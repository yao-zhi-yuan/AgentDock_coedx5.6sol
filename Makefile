SHELL := /bin/sh

GO_TOOLCHAIN := go1.26.5
GOLANGCI_LINT_VERSION := v2.12.2
COMPOSE_FILE := deployments/docker-compose.yml
DOCTOR_ENV := .env.example

.DEFAULT_GOAL := help

.PHONY: help doctor compose-config \
	lint test test-race test-integration test-chaos security-test \
	migrate demo-fake demo-eino-recorded sandbox-security-test \
	chaos-worker-kill test-rebuild-state e2e-recorded e2e-replay e2e-live

help:
	@echo "AgentDock Verify — phase 0 repository skeleton"
	@echo "  make doctor          Validate the phase 0 development environment"
	@echo "  make compose-config  Parse the pinned Docker Compose configuration"
	@echo "  Other targets are intentionally unavailable until their planned phase"

doctor:
	@EXPECTED_GO_TOOLCHAIN="$(GO_TOOLCHAIN)" DOCTOR_ENV="$(DOCTOR_ENV)" COMPOSE_FILE="$(COMPOSE_FILE)" ./scripts/doctor.sh

compose-config:
	@docker compose --env-file "$(DOCTOR_ENV)" -f "$(COMPOSE_FILE)" config --quiet

lint:
	@./scripts/not-implemented.sh "lint" "phase 1"

test:
	@./scripts/not-implemented.sh "test" "phase 1"

test-race:
	@./scripts/not-implemented.sh "test-race" "phase 1"

test-integration:
	@./scripts/not-implemented.sh "test-integration" "phase 2"

test-chaos:
	@./scripts/not-implemented.sh "test-chaos" "phase 3"

security-test:
	@./scripts/not-implemented.sh "security-test" "phase 4"

migrate:
	@./scripts/not-implemented.sh "migrate" "phase 2"

demo-fake:
	@./scripts/not-implemented.sh "demo-fake" "phase 1"

demo-eino-recorded:
	@./scripts/not-implemented.sh "demo-eino-recorded" "phase 5"

sandbox-security-test:
	@./scripts/not-implemented.sh "sandbox-security-test" "phase 4"

chaos-worker-kill:
	@./scripts/not-implemented.sh "chaos-worker-kill" "phase 3"

test-rebuild-state:
	@./scripts/not-implemented.sh "test-rebuild-state" "phase 2"

e2e-recorded:
	@./scripts/not-implemented.sh "e2e-recorded" "phase 6"

e2e-replay:
	@./scripts/not-implemented.sh "e2e-replay" "phase 7"

e2e-live:
	@./scripts/not-implemented.sh "e2e-live" "phase 5 (optional, never a CI gate)"
