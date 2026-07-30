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

func TestSessionRunsRequiredCLIChainInOneProcess(t *testing.T) {
	runtime := controller.New(store.NewMemoryEventStore(), reasoner.NewFakeReasoner())
	input := strings.NewReader(strings.Join([]string{
		"run create run-session --scenario scenario --spec-hash spec",
		"run get run-session",
		"run step run-session",
		"run pause run-session",
		"run resume run-session",
		"run cancel run-session",
	}, "\n") + "\n")
	var output bytes.Buffer

	root := cli.NewRootCommand(runtime)
	root.SetArgs([]string{"session"})
	root.SetIn(input)
	root.SetOut(&output)
	root.SetErr(&output)
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("agentdock session error = %v\n%s", err, output.String())
	}

	got := output.String()
	for _, expected := range []string{
		`>>> run create run-session --scenario scenario --spec-hash spec`,
		`"observed_state": "Queued"`,
		`"command": "StartAttempt"`,
		`"observed_state": "Provisioning"`,
		`"observed_state": "Paused"`,
		`"observed_state": "Cancelled"`,
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("session output does not contain %q:\n%s", expected, got)
		}
	}
}
