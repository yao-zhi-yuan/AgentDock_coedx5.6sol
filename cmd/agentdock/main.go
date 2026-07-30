package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/agentdock/agentdock-verify/internal/cli"
	"github.com/agentdock/agentdock-verify/internal/controller"
	"github.com/agentdock/agentdock-verify/internal/reasoner"
	"github.com/agentdock/agentdock-verify/internal/store"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, input io.Reader, output, errorOutput io.Writer) int {
	eventStore := store.EventStore(store.NewMemoryEventStore())
	closeStore := func() {}
	databaseURL := os.Getenv("AGENTDOCK_DATABASE_URL")
	if databaseURL == "" && len(args) > 0 && args[0] == "run" {
		fmt.Fprintln(errorOutput, "AGENTDOCK_DATABASE_URL is required for durable run commands")
		return 1
	}
	if databaseURL != "" && (len(args) == 0 || args[0] != "demo-fake") {
		postgres, err := store.NewPostgresEventStore(context.Background(), databaseURL)
		if err != nil {
			fmt.Fprintln(errorOutput, err)
			return 1
		}
		eventStore = postgres
		closeStore = postgres.Close
	}
	defer closeStore()

	runtime := controller.New(eventStore, reasoner.NewFakeReasoner())
	root := cli.NewRootCommand(runtime)
	root.SetArgs(args)
	root.SetIn(input)
	root.SetOut(output)
	root.SetErr(errorOutput)
	if err := root.ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(errorOutput, err)
		return 1
	}
	return 0
}
