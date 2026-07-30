package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunEntryPointKeepsSessionStateForRequiredCommands(t *testing.T) {
	t.Setenv("AGENTDOCK_DATABASE_URL", "")
	input := strings.NewReader(strings.Join([]string{
		"run create run-entry --scenario scenario --spec-hash spec",
		"run get run-entry",
		"run step run-entry",
		"run pause run-entry",
		"run resume run-entry",
		"run cancel run-entry",
	}, "\n") + "\n")
	var output bytes.Buffer
	var errorOutput bytes.Buffer

	exitCode := run([]string{"session"}, input, &output, &errorOutput)
	if exitCode != 0 {
		t.Fatalf("run() exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", exitCode, output.String(), errorOutput.String())
	}
	if !strings.Contains(output.String(), `"observed_state": "Cancelled"`) {
		t.Fatalf("entrypoint did not finish command chain:\n%s", output.String())
	}
}

func TestStandaloneRunCommandsRequireDatabaseAuthority(t *testing.T) {
	t.Setenv("AGENTDOCK_DATABASE_URL", "")
	var output bytes.Buffer
	var errorOutput bytes.Buffer

	exitCode := run([]string{"run", "get", "missing"}, strings.NewReader(""), &output, &errorOutput)
	if exitCode != 1 {
		t.Fatalf("run() exit = %d, want 1", exitCode)
	}
	if !strings.Contains(errorOutput.String(), "AGENTDOCK_DATABASE_URL is required") {
		t.Fatalf("run() error = %q, want database authority requirement", errorOutput.String())
	}
}
