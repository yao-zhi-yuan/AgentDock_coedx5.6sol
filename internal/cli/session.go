package cli

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/agentdock/agentdock-verify/internal/controller"
	"github.com/spf13/cobra"
)

func newSessionCommand(runtime *controller.Controller) *cobra.Command {
	return &cobra.Command{
		Use:   "session",
		Short: "Run newline-delimited CLI commands in one in-memory process",
		Long: "Run newline-delimited CLI commands in one in-memory process.\n" +
			"Arguments are whitespace-delimited; quoted values are not supported in phase 1.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			scanner := bufio.NewScanner(command.InOrStdin())
			output := command.OutOrStdout()
			lineNumber := 0
			for scanner.Scan() {
				lineNumber++
				line := strings.TrimSpace(scanner.Text())
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				args := strings.Fields(line)
				if len(args) > 0 && args[0] == "session" {
					return fmt.Errorf("session line %d: nested session is not allowed", lineNumber)
				}

				fmt.Fprintf(output, ">>> %s\n", line)
				nested := NewRootCommand(runtime)
				nested.SetArgs(args)
				nested.SetIn(command.InOrStdin())
				nested.SetOut(output)
				nested.SetErr(command.ErrOrStderr())
				if err := nested.ExecuteContext(command.Context()); err != nil {
					return fmt.Errorf("session line %d: %w", lineNumber, err)
				}
			}
			if err := scanner.Err(); err != nil {
				return fmt.Errorf("read session: %w", err)
			}
			return nil
		},
	}
}
