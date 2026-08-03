package reasoner_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestReasonerHasNoDatabaseOrHostFileWriteImportsAndEinoIsAdapterOnly(t *testing.T) {
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	reasonerRoot := filepath.Join(repository, "internal", "reasoner")
	forbiddenReasonerImports := map[string]bool{
		"database/sql":  true,
		"os":            true,
		"os/exec":       true,
		"path/filepath": true,
		"syscall":       true,
		"github.com/agentdock/agentdock-verify/internal/store":   true,
		"github.com/agentdock/agentdock-verify/internal/sandbox": true,
	}

	err = filepath.WalkDir(repository, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, importSpec := range parsed.Imports {
			importPath, err := strconv.Unquote(importSpec.Path.Value)
			if err != nil {
				return err
			}
			if strings.HasPrefix(path, reasonerRoot+string(filepath.Separator)) && forbiddenReasonerImports[importPath] {
				t.Errorf("Reasoner production file %s imports forbidden authority %q", path, importPath)
			}
			if strings.HasPrefix(importPath, "github.com/cloudwego/eino") {
				adapterRoot := filepath.Join(reasonerRoot, "eino") + string(filepath.Separator)
				if !strings.HasPrefix(path, adapterRoot) {
					t.Errorf("Eino import escaped adapter package: %s imports %q", path, importPath)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	controllerPath := filepath.Join(repository, "internal", "controller", "controller.go")
	parsed, err := parser.ParseFile(token.NewFileSet(), controllerPath, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, importSpec := range parsed.Imports {
		path, _ := strconv.Unquote(importSpec.Path.Value)
		if strings.HasPrefix(path, "github.com/cloudwego/eino") {
			t.Errorf("Controller imports Eino concrete type %q", path)
		}
	}
}
