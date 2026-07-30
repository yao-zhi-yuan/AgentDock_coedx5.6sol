#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_root"

compose_file=${COMPOSE_FILE:-deployments/docker-compose.yml}
env_file=${DOCTOR_ENV:-.env.example}
expected_go=${EXPECTED_GO_TOOLCHAIN:-go1.26.5}

pass() {
	printf '[doctor] PASS: %s\n' "$1"
}

fail() {
	printf '[doctor] FAIL: %s\n' "$1" >&2
	exit 1
}

require_command() {
	command -v "$1" >/dev/null 2>&1 || fail "$1 is not installed or not on PATH"
}

env_value() {
	key=$1
	sed -n "s/^${key}=//p" "$env_file" | tail -n 1
}

require_env_key() {
	key=$1
	[ -n "$(env_value "$key")" ] || fail "$env_file is missing non-empty $key"
}

require_port() {
	key=$1
	port=$(env_value "$key")
	case "$port" in
		''|*[!0-9]*)
			fail "$env_file has invalid $key; expected an integer from 1 to 65535"
			;;
	esac
	[ "$port" -ge 1 ] && [ "$port" -le 65535 ] ||
		fail "$env_file has invalid $key; expected an integer from 1 to 65535"
}

port_is_listening() {
	port=$1
	if command -v lsof >/dev/null 2>&1; then
		lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1
		return
	fi
	if command -v nc >/dev/null 2>&1; then
		nc -z 127.0.0.1 "$port" >/dev/null 2>&1
		return
	fi
	fail "neither lsof nor nc is available for port checks"
}

require_command go
actual_go=$(go env GOVERSION)
[ "$actual_go" = "$expected_go" ] || fail "selected Go toolchain is $actual_go; expected $expected_go"
pass "Go is available ($actual_go)"

require_command docker
docker version >/dev/null 2>&1 || fail "Docker daemon is not reachable"
pass "Docker daemon is reachable"

docker compose version >/dev/null 2>&1 || fail "Docker Compose is unavailable"
compose_version=$(docker compose version --short)
pass "Docker Compose is available ($compose_version)"

[ -f "$env_file" ] || fail "$env_file does not exist"
for key in \
	AGENTDOCK_MODE \
	AGENTDOCK_DATABASE_URL \
	AGENTDOCK_ARTIFACT_DIR \
	POSTGRES_DB \
	POSTGRES_USER \
	POSTGRES_PASSWORD \
	POSTGRES_PORT \
	OTEL_GRPC_PORT \
	OTEL_HTTP_PORT \
	JAEGER_UI_PORT
do
	require_env_key "$key"
done
pass "all required phase 0 parameters have non-empty example values"

checked_ports=
checked_port_list=
for key in POSTGRES_PORT OTEL_GRPC_PORT OTEL_HTTP_PORT JAEGER_UI_PORT
do
	require_port "$key"
	port=$(env_value "$key")
	case " $checked_ports " in
		*" $port "*)
			fail "$env_file assigns duplicate host port $port"
			;;
	esac
	checked_ports="$checked_ports $port"
	checked_port_list="${checked_port_list}${checked_port_list:+, }$port"
	if port_is_listening "$port"; then
		fail "$key requires local port $port, which is already in use"
	fi
done
pass "configured local ports $checked_port_list are valid, distinct, and free"

go run ./scripts/configcheck \
	configs/agentdock.yaml \
	configs/otel-collector.yaml \
	configs/architecture-rules.example.yaml \
	examples/scenarios/harness-run.example.yaml \
	.golangci.yml ||
	fail "project YAML validation failed"
pass "project YAML files parse successfully"

docker compose --env-file "$env_file" -f "$compose_file" config --quiet ||
	fail "Docker Compose configuration failed to parse with $env_file"
pass "Docker Compose configuration parses with $env_file"

printf '[doctor] PASS: all phase 0 environment checks completed\n'
