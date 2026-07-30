package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/agentdock/agentdock-verify/internal/controller"
	"github.com/agentdock/agentdock-verify/internal/domain"
	"github.com/spf13/cobra"
)

func newDemoFakeCommand(runtime *controller.Controller) *cobra.Command {
	return &cobra.Command{
		Use:   "demo-fake",
		Short: "Run deterministic success and pause/resume demonstrations",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			ctx := command.Context()
			output := command.OutOrStdout()

			fmt.Fprintln(output, "=== successful fake Run ===")
			if _, err := runtime.CreateRun(ctx, controller.CreateRunRequest{
				RunID:      "demo-success",
				ScenarioID: "phase-1-success",
				SpecHash:   "phase-1-spec",
			}); err != nil {
				return err
			}
			if err := printEvents(ctx, output, runtime, "demo-success", 0); err != nil {
				return err
			}
			printed := 1
			for {
				result, err := runtime.Reconcile(ctx, "demo-success")
				if err != nil {
					return err
				}
				for _, event := range result.Events {
					fmt.Fprintln(output, event.Type)
					printed++
				}
				if result.State.Run.ObservedState.Terminal() {
					fmt.Fprintf(output, "FinalState=%s Events=%d\n", result.State.Run.ObservedState, printed)
					break
				}
			}

			fmt.Fprintln(output, "=== pause / resume ===")
			if _, err := runtime.CreateRun(ctx, controller.CreateRunRequest{
				RunID:      "demo-pause-resume",
				ScenarioID: "phase-1-pause-resume",
				SpecHash:   "phase-1-spec",
			}); err != nil {
				return err
			}
			first, err := runtime.Reconcile(ctx, "demo-pause-resume")
			if err != nil {
				return err
			}
			fmt.Fprintf(output, "BeforePause=%s\n", first.State.Run.ObservedState)
			paused, err := runtime.SetDesiredState(ctx, "demo-pause-resume", domain.DesiredPaused)
			if err != nil {
				return err
			}
			fmt.Fprintf(output, "Paused=%s ResumeTarget=%s\n", paused.Run.ObservedState, paused.ResumeState)
			for range 10 {
				result, reconcileErr := runtime.Reconcile(ctx, "demo-pause-resume")
				if reconcileErr != nil {
					return reconcileErr
				}
				if result.Command.Type != domain.CommandNoop || len(result.Events) != 0 {
					return fmt.Errorf("paused reconcile produced command %s or %d events", result.Command.Type, len(result.Events))
				}
			}
			resumed, err := runtime.SetDesiredState(ctx, "demo-pause-resume", domain.DesiredRunning)
			if err != nil {
				return err
			}
			fmt.Fprintf(output, "Resumed=%s\n", resumed.Run.ObservedState)
			for !resumed.Run.ObservedState.Terminal() {
				result, reconcileErr := runtime.Reconcile(ctx, "demo-pause-resume")
				if reconcileErr != nil {
					return reconcileErr
				}
				resumed = result.State
			}
			fmt.Fprintf(output, "PauseResumeFinal=%s\n", resumed.Run.ObservedState)
			return nil
		},
	}
}

func printEvents(
	ctx context.Context,
	output io.Writer,
	runtime *controller.Controller,
	runID string,
	from int,
) error {
	events, err := runtime.Events(ctx, runID)
	if err != nil {
		return err
	}
	for _, event := range events[from:] {
		fmt.Fprintln(output, event.Type)
	}
	return nil
}
