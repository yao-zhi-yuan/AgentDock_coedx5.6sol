package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/agentdock/agentdock-verify/internal/controller"
	"github.com/agentdock/agentdock-verify/internal/domain"
	"github.com/spf13/cobra"
)

// NewRootCommand builds the CLI around an injected Controller. The same
// command path works with the memory and PostgreSQL stores.
func NewRootCommand(runtime *controller.Controller) *cobra.Command {
	root := &cobra.Command{
		Use:           "agentdock",
		Short:         "AgentDock Verify durable runtime",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newRunCommand(runtime), newSessionCommand(runtime), newDemoFakeCommand(runtime))
	return root
}

func newRunCommand(runtime *controller.Controller) *cobra.Command {
	run := &cobra.Command{Use: "run", Short: "Manage Runs"}
	run.AddCommand(
		newCreateCommand(runtime),
		newGetCommand(runtime),
		newEventsCommand(runtime),
		newStepCommand(runtime),
		newDesiredCommand(runtime, "pause", domain.DesiredPaused),
		newDesiredCommand(runtime, "resume", domain.DesiredRunning),
		newCancelCommand(runtime),
	)
	return run
}

func newEventsCommand(runtime *controller.Controller) *cobra.Command {
	return &cobra.Command{
		Use:   "events RUN_ID",
		Short: "Display the authoritative ordered Event Log",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			events, err := runtime.Events(command.Context(), args[0])
			if err != nil {
				return err
			}
			return writeJSON(command.OutOrStdout(), events)
		},
	}
}

func newCreateCommand(runtime *controller.Controller) *cobra.Command {
	var scenarioID string
	var specHash string
	command := &cobra.Command{
		Use:   "create RUN_ID",
		Short: "Create a Run",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			state, err := runtime.CreateRun(command.Context(), controller.CreateRunRequest{
				RunID:      args[0],
				ScenarioID: scenarioID,
				SpecHash:   specHash,
			})
			if err != nil {
				return err
			}
			return writeJSON(command.OutOrStdout(), state)
		},
	}
	command.Flags().StringVar(&scenarioID, "scenario", "", "scenario identifier (required)")
	command.Flags().StringVar(&specHash, "spec-hash", "", "specification digest (required)")
	_ = command.MarkFlagRequired("scenario")
	_ = command.MarkFlagRequired("spec-hash")
	return command
}

func newGetCommand(runtime *controller.Controller) *cobra.Command {
	return &cobra.Command{
		Use:   "get RUN_ID",
		Short: "Reduce and display a Run",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			state, err := runtime.GetRun(command.Context(), args[0])
			if err != nil {
				return err
			}
			return writeJSON(command.OutOrStdout(), state)
		},
	}
}

func newStepCommand(runtime *controller.Controller) *cobra.Command {
	return &cobra.Command{
		Use:   "step RUN_ID",
		Short: "Reconcile one command",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			result, err := runtime.Reconcile(command.Context(), args[0])
			if err != nil {
				return err
			}
			return writeJSON(command.OutOrStdout(), struct {
				Command domain.CommandType `json:"command"`
				State   domain.State       `json:"state"`
				Events  []domain.Event     `json:"events"`
			}{
				Command: result.Command.Type,
				State:   result.State,
				Events:  result.Events,
			})
		},
	}
}

func newDesiredCommand(runtime *controller.Controller, name string, desired domain.DesiredState) *cobra.Command {
	return &cobra.Command{
		Use:   name + " RUN_ID",
		Short: fmt.Sprintf("Set desired state to %s", desired),
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			state, err := runtime.SetDesiredState(command.Context(), args[0], desired)
			if err != nil {
				return err
			}
			return writeJSON(command.OutOrStdout(), state)
		},
	}
}

func newCancelCommand(runtime *controller.Controller) *cobra.Command {
	return &cobra.Command{
		Use:   "cancel RUN_ID",
		Short: "Persist cancel intent and converge the Run",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			state, err := runtime.SetDesiredState(command.Context(), args[0], domain.DesiredCancelled)
			if err != nil {
				return err
			}
			result, err := runtime.Reconcile(command.Context(), args[0])
			if errors.Is(err, controller.ErrManagedRunRequiresLease) {
				return writeJSON(command.OutOrStdout(), state)
			}
			if err != nil {
				return err
			}
			return writeJSON(command.OutOrStdout(), result.State)
		},
	}
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
