//go:build integration

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/agentdock/agentdock-verify/internal/domain"
)

func TestCLIContinuesRunAcrossSeparateProcesses(t *testing.T) {
	databaseURL := os.Getenv("AGENTDOCK_DATABASE_URL")
	if databaseURL == "" {
		databaseURL = "postgres://agentdock:agentdock_dev_only@127.0.0.1:55433/agentdock?sslmode=disable"
	}
	runID := fmt.Sprintf("run-cli-process-%d", time.Now().UnixNano())

	var created domain.State
	runCLIProcess(
		t,
		databaseURL,
		&created,
		"run", "create", runID, "--scenario", "process-restart", "--spec-hash", "phase-2",
	)
	if created.Run.Version != 1 || created.Run.ObservedState != domain.StatusQueued {
		t.Fatalf("created State = %#v, want version 1 Queued", created)
	}

	var lastState domain.State
	for step := 0; step < 12; step++ {
		var result struct {
			State domain.State `json:"state"`
		}
		runCLIProcess(t, databaseURL, &result, "run", "step", runID)
		lastState = result.State
		if lastState.Run.ObservedState == domain.StatusSucceeded {
			break
		}
	}
	if lastState.Run.ObservedState != domain.StatusSucceeded {
		t.Fatalf("separate CLI processes ended at %#v, want Succeeded", lastState)
	}

	var events []domain.Event
	runCLIProcess(t, databaseURL, &events, "run", "events", runID)
	if len(events) != int(lastState.Run.Version) {
		t.Fatalf("events=%d State.version=%d", len(events), lastState.Run.Version)
	}
	for index, event := range events {
		if event.Seq != uint64(index+1) {
			t.Fatalf("events[%d].seq=%d, want %d", index, event.Seq, index+1)
		}
	}

	var rebuilt domain.State
	runCLIProcess(t, databaseURL, &rebuilt, "run", "get", runID)
	if rebuilt != lastState {
		t.Fatalf("fresh-process rebuilt State differs\n got: %#v\nwant: %#v", rebuilt, lastState)
	}
}

func TestCLIHelperProcess(t *testing.T) {
	if os.Getenv("AGENTDOCK_CLI_HELPER_PROCESS") != "1" {
		return
	}
	separator := -1
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator == -1 || separator+1 >= len(os.Args) {
		fmt.Fprintln(os.Stderr, "helper process is missing CLI arguments")
		os.Exit(2)
	}
	os.Exit(run(os.Args[separator+1:], os.Stdin, os.Stdout, os.Stderr))
}

func runCLIProcess(
	t *testing.T,
	databaseURL string,
	target any,
	arguments ...string,
) {
	t.Helper()
	helperArguments := append(
		[]string{"-test.run=^TestCLIHelperProcess$", "--"},
		arguments...,
	)
	command := exec.Command(os.Args[0], helperArguments...)
	command.Env = append(
		os.Environ(),
		"AGENTDOCK_CLI_HELPER_PROCESS=1",
		"AGENTDOCK_DATABASE_URL="+databaseURL,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf(
			"separate process agentdock %s error = %v\n%s",
			strings.Join(arguments, " "),
			err,
			output,
		)
	}
	if err := json.Unmarshal(output, target); err != nil {
		t.Fatalf(
			"decode separate process agentdock %s output: %v\n%s",
			strings.Join(arguments, " "),
			err,
			output,
		)
	}
}
