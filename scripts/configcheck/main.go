// Command configcheck performs phase-0 YAML syntax validation.
//
// It is repository tooling, not AgentDock runtime or domain implementation.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"go.yaml.in/yaml/v3"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: configcheck <yaml-file>...")
		os.Exit(2)
	}

	for _, path := range os.Args[1:] {
		if err := check(path); err != nil {
			fmt.Fprintf(os.Stderr, "configcheck: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("[configcheck] PASS: %s\n", path)
	}
}

func check(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%s: open: %w", path, err)
	}
	defer file.Close()

	decoder := yaml.NewDecoder(file)
	documents := 0
	for {
		var root yaml.Node
		err = decoder.Decode(&root)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%s: parse: %w", path, err)
		}
		if len(root.Content) == 0 {
			continue
		}
		documents++
		if root.Content[0].Kind != yaml.MappingNode {
			return fmt.Errorf("%s: document %d root must be a mapping", path, documents)
		}
	}

	if documents == 0 {
		return fmt.Errorf("%s: contains no YAML document", path)
	}
	return nil
}
