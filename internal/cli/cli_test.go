package cli_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/agentdock/agentdock-verify/internal/cli"
	"github.com/agentdock/agentdock-verify/internal/controller"
	"github.com/agentdock/agentdock-verify/internal/reasoner"
	"github.com/agentdock/agentdock-verify/internal/store"
)

func TestRunCommandsShareInjectedInMemoryRuntime(t *testing.T) {
	runtime := controller.New(store.NewMemoryEventStore(), reasoner.NewFakeReasoner())

	output := execute(t, runtime, "run", "create", "run-cli", "--scenario", "scenario", "--spec-hash", "spec")
	if !strings.Contains(output, `"observed_state": "Queued"`) {
		t.Fatalf("create output = %s", output)
	}

	output = execute(t, runtime, "run", "get", "run-cli")
	if !strings.Contains(output, `"run_id": "run-cli"`) {
		t.Fatalf("get output = %s", output)
	}

	output = execute(t, runtime, "run", "step", "run-cli")
	if !strings.Contains(output, `"command": "StartAttempt"`) ||
		!strings.Contains(output, `"observed_state": "Provisioning"`) {
		t.Fatalf("step output = %s", output)
	}
	output = execute(t, runtime, "run", "events", "run-cli")
	if !strings.Contains(output, `"seq": 1`) ||
		!strings.Contains(output, `"seq": 2`) ||
		!strings.Contains(output, `"event_type": "AttemptStarted"`) {
		t.Fatalf("events output = %s", output)
	}

	output = execute(t, runtime, "run", "pause", "run-cli")
	if !strings.Contains(output, `"observed_state": "Paused"`) {
		t.Fatalf("pause output = %s", output)
	}

	output = execute(t, runtime, "run", "resume", "run-cli")
	if !strings.Contains(output, `"observed_state": "Provisioning"`) {
		t.Fatalf("resume output = %s", output)
	}

	output = execute(t, runtime, "run", "cancel", "run-cli")
	if !strings.Contains(output, `"observed_state": "Cancelled"`) {
		t.Fatalf("cancel output = %s", output)
	}
}

func TestRunCreateRequiresExplicitID(t *testing.T) {
	runtime := controller.New(store.NewMemoryEventStore(), reasoner.NewFakeReasoner())
	root := cli.NewRootCommand(runtime)
	root.SetArgs([]string{"run", "create"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})

	if err := root.ExecuteContext(context.Background()); err == nil {
		t.Fatal("run create without ID unexpectedly succeeded")
	}
}

func execute(t *testing.T, runtime *controller.Controller, args ...string) string {
	t.Helper()
	var output bytes.Buffer
	root := cli.NewRootCommand(runtime)
	root.SetArgs(args)
	root.SetOut(&output)
	root.SetErr(&output)
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("agentdock %s error = %v\n%s", strings.Join(args, " "), err, output.String())
	}
	return output.String()
}
