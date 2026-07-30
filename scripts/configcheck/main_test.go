package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckAcceptsMappingDocument(t *testing.T) {
	path := writeConfig(t, "apiVersion: agentdock.dev/v1alpha1\nkind: RuntimeConfig\n")

	if err := check(path); err != nil {
		t.Fatalf("check() returned an unexpected error: %v", err)
	}
}

func TestCheckRejectsMalformedYAML(t *testing.T) {
	path := writeConfig(t, "key: [unterminated\n")

	if err := check(path); err == nil {
		t.Fatal("check() accepted malformed YAML")
	}
}

func TestCheckRejectsScalarRoot(t *testing.T) {
	path := writeConfig(t, "not-a-mapping\n")

	if err := check(path); err == nil {
		t.Fatal("check() accepted a scalar root")
	}
}

func TestCheckRejectsEmptyFile(t *testing.T) {
	path := writeConfig(t, "\n")

	if err := check(path); err == nil {
		t.Fatal("check() accepted an empty file")
	}
}

func writeConfig(t *testing.T, contents string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	return path
}
