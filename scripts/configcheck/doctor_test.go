package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorAcceptsValidEnvironment(t *testing.T) {
	result := runDoctor(t, readExampleEnv(t), nil)

	if result.err != nil {
		t.Fatalf("doctor failed: %v\n%s", result.err, result.output)
	}
	if !strings.Contains(result.output, "configured local ports 55433, 14317, 14318, 16687 are valid, distinct, and available to this Compose project") {
		t.Fatalf("doctor did not report configured port checks:\n%s", result.output)
	}
}

func TestDoctorRejectsMissingRequiredParameter(t *testing.T) {
	envFile := strings.Replace(readExampleEnv(t), "AGENTDOCK_MODE=recorded", "AGENTDOCK_MODE=", 1)
	result := runDoctor(t, envFile, nil)

	requireDoctorFailure(t, result, "missing non-empty AGENTDOCK_MODE")
}

func TestDoctorRejectsInvalidPort(t *testing.T) {
	envFile := strings.Replace(readExampleEnv(t), "POSTGRES_PORT=55433", "POSTGRES_PORT=not-a-port", 1)
	result := runDoctor(t, envFile, nil)

	requireDoctorFailure(t, result, "invalid POSTGRES_PORT")
}

func TestDoctorRejectsDuplicatePorts(t *testing.T) {
	envFile := strings.Replace(readExampleEnv(t), "OTEL_GRPC_PORT=14317", "OTEL_GRPC_PORT=55433", 1)
	result := runDoctor(t, envFile, nil)

	requireDoctorFailure(t, result, "duplicate host port 55433")
}

func TestDoctorChecksConfiguredPortForConflict(t *testing.T) {
	envFile := strings.Replace(readExampleEnv(t), "POSTGRES_PORT=55433", "POSTGRES_PORT=15432", 1)
	result := runDoctor(t, envFile, map[string]string{"FAKE_BUSY_PORT": "15432"})

	requireDoctorFailure(t, result, "POSTGRES_PORT requires local port 15432")
}

func TestDoctorAcceptsBusyPostgresPortOwnedByComposeService(t *testing.T) {
	envFile := strings.Replace(readExampleEnv(t), "POSTGRES_PORT=55433", "POSTGRES_PORT=15432", 1)
	result := runDoctor(t, envFile, map[string]string{
		"FAKE_BUSY_PORT":             "15432",
		"FAKE_COMPOSE_POSTGRES_PORT": "15432",
	})

	if result.err != nil {
		t.Fatalf("doctor rejected the Compose-owned PostgreSQL port: %v\n%s", result.err, result.output)
	}
}

func TestDoctorRejectsBusyPortPublishedByDifferentComposePort(t *testing.T) {
	envFile := strings.Replace(readExampleEnv(t), "POSTGRES_PORT=55433", "POSTGRES_PORT=15432", 1)
	result := runDoctor(t, envFile, map[string]string{
		"FAKE_BUSY_PORT":             "15432",
		"FAKE_COMPOSE_POSTGRES_PORT": "25432",
	})

	requireDoctorFailure(t, result, "POSTGRES_PORT requires local port 15432")
}

func TestDoctorAcceptsAllBusyPortsOwnedByExpectedComposeServices(t *testing.T) {
	envFile := readExampleEnv(t)
	envFile = strings.Replace(envFile, "POSTGRES_PORT=55433", "POSTGRES_PORT=15432", 1)
	envFile = strings.Replace(envFile, "OTEL_GRPC_PORT=14317", "OTEL_GRPC_PORT=24317", 1)
	envFile = strings.Replace(envFile, "OTEL_HTTP_PORT=14318", "OTEL_HTTP_PORT=24318", 1)
	envFile = strings.Replace(envFile, "JAEGER_UI_PORT=16687", "JAEGER_UI_PORT=26686", 1)
	result := runDoctor(t, envFile, map[string]string{
		"FAKE_BUSY_PORTS":             "15432 24317 24318 26686",
		"FAKE_COMPOSE_POSTGRES_PORT":  "15432",
		"FAKE_COMPOSE_OTEL_GRPC_PORT": "24317",
		"FAKE_COMPOSE_OTEL_HTTP_PORT": "24318",
		"FAKE_COMPOSE_JAEGER_UI_PORT": "26686",
	})

	if result.err != nil {
		t.Fatalf("doctor rejected ports owned by the expected Compose services: %v\n%s", result.err, result.output)
	}
}

func TestDoctorRejectsBusyTelemetryPortOwnedElsewhere(t *testing.T) {
	envFile := strings.Replace(readExampleEnv(t), "OTEL_GRPC_PORT=14317", "OTEL_GRPC_PORT=24317", 1)
	result := runDoctor(t, envFile, map[string]string{
		"FAKE_BUSY_PORTS":             "24317",
		"FAKE_COMPOSE_OTEL_GRPC_PORT": "34317",
	})

	requireDoctorFailure(t, result, "OTEL_GRPC_PORT requires local port 24317")
}

func TestDoctorRejectsWrongGoToolchain(t *testing.T) {
	result := runDoctor(t, readExampleEnv(t), map[string]string{"FAKE_GO_VERSION": "go1.25.0"})

	requireDoctorFailure(t, result, "selected Go toolchain is go1.25.0; expected go1.26.5")
}

func TestDoctorRejectsUnavailableDockerDaemon(t *testing.T) {
	result := runDoctor(t, readExampleEnv(t), map[string]string{"FAKE_DOCKER_FAIL": "1"})

	requireDoctorFailure(t, result, "Docker daemon is not reachable")
}

func TestDoctorRejectsUnavailableCompose(t *testing.T) {
	result := runDoctor(t, readExampleEnv(t), map[string]string{"FAKE_COMPOSE_VERSION_FAIL": "1"})

	requireDoctorFailure(t, result, "Docker Compose is unavailable")
}

func TestDoctorRejectsInvalidComposeModel(t *testing.T) {
	result := runDoctor(t, readExampleEnv(t), map[string]string{"FAKE_COMPOSE_CONFIG_FAIL": "1"})

	requireDoctorFailure(t, result, "Docker Compose configuration")
}

type doctorResult struct {
	output string
	err    error
}

func runDoctor(t *testing.T, envFile string, overrides map[string]string) doctorResult {
	t.Helper()

	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	tempDir := t.TempDir()
	envPath := filepath.Join(tempDir, "doctor.env")
	if err := os.WriteFile(envPath, []byte(envFile), 0o600); err != nil {
		t.Fatalf("write doctor env: %v", err)
	}

	binDir := filepath.Join(tempDir, "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatalf("create fake bin: %v", err)
	}
	writeExecutable(t, filepath.Join(binDir, "go"), fakeGo)
	writeExecutable(t, filepath.Join(binDir, "docker"), fakeDocker)
	writeExecutable(t, filepath.Join(binDir, "lsof"), fakeLsof)

	command := exec.Command("sh", "scripts/doctor.sh")
	command.Dir = repoRoot
	command.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"DOCTOR_ENV="+envPath,
		"COMPOSE_FILE=deployments/docker-compose.yml",
		"EXPECTED_GO_TOOLCHAIN=go1.26.5",
	)
	for key, value := range overrides {
		command.Env = append(command.Env, key+"="+value)
	}

	output, err := command.CombinedOutput()
	return doctorResult{output: string(output), err: err}
}

func readExampleEnv(t *testing.T) string {
	t.Helper()

	contents, err := os.ReadFile(filepath.Join("..", "..", ".env.example"))
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}
	return string(contents)
}

func requireDoctorFailure(t *testing.T, result doctorResult, message string) {
	t.Helper()

	if result.err == nil {
		t.Fatalf("doctor unexpectedly passed:\n%s", result.output)
	}
	if !strings.Contains(result.output, message) {
		t.Fatalf("doctor output does not contain %q:\n%s", message, result.output)
	}
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

const fakeGo = `#!/bin/sh
if [ "$1" = "env" ] && [ "$2" = "GOVERSION" ]; then
	printf '%s\n' "${FAKE_GO_VERSION:-go1.26.5}"
	exit 0
fi
if [ "$1" = "run" ]; then
	exit "${FAKE_GO_RUN_FAIL:-0}"
fi
exit 0
`

const fakeDocker = `#!/bin/sh
if [ "$1" = "version" ]; then
	exit "${FAKE_DOCKER_FAIL:-0}"
fi
if [ "$1" = "compose" ] && [ "$2" = "version" ]; then
	if [ "${3:-}" = "--short" ]; then
		printf '5.1.3\n'
	fi
	exit "${FAKE_COMPOSE_VERSION_FAIL:-0}"
fi
if [ "$1" = "compose" ]; then
	case "$*" in
		*" ps --status running --services")
			if [ -n "${FAKE_COMPOSE_POSTGRES_PORT:-}" ]; then
				printf 'postgres\n'
			fi
			if [ -n "${FAKE_COMPOSE_OTEL_GRPC_PORT:-}${FAKE_COMPOSE_OTEL_HTTP_PORT:-}" ]; then
				printf 'otel-collector\n'
			fi
			if [ -n "${FAKE_COMPOSE_JAEGER_UI_PORT:-}" ]; then
				printf 'jaeger\n'
			fi
			exit 0
			;;
		*" port postgres 5432")
			if [ -n "${FAKE_COMPOSE_POSTGRES_PORT:-}" ]; then
				printf '127.0.0.1:%s\n' "$FAKE_COMPOSE_POSTGRES_PORT"
				exit 0
			fi
			exit 1
			;;
		*" port otel-collector 4317")
			if [ -n "${FAKE_COMPOSE_OTEL_GRPC_PORT:-}" ]; then
				printf '127.0.0.1:%s\n' "$FAKE_COMPOSE_OTEL_GRPC_PORT"
				exit 0
			fi
			exit 1
			;;
		*" port otel-collector 4318")
			if [ -n "${FAKE_COMPOSE_OTEL_HTTP_PORT:-}" ]; then
				printf '127.0.0.1:%s\n' "$FAKE_COMPOSE_OTEL_HTTP_PORT"
				exit 0
			fi
			exit 1
			;;
		*" port jaeger 16686")
			if [ -n "${FAKE_COMPOSE_JAEGER_UI_PORT:-}" ]; then
				printf '127.0.0.1:%s\n' "$FAKE_COMPOSE_JAEGER_UI_PORT"
				exit 0
			fi
			exit 1
			;;
	esac
	exit "${FAKE_COMPOSE_CONFIG_FAIL:-0}"
fi
exit 1
`

const fakeLsof = `#!/bin/sh
for port in ${FAKE_BUSY_PORTS:-${FAKE_BUSY_PORT:-never}}; do
	case "$*" in
		*":${port} "*)
			exit 0
			;;
	esac
done
exit 1
`
