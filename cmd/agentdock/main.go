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
	runtime := controller.New(store.NewMemoryEventStore(), reasoner.NewFakeReasoner())
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
