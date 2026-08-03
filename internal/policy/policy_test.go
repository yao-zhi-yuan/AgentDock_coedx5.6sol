package policy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPolicyDefaultsToDenyAndAuditsTheDecision(t *testing.T) {
	recorder := NewMemoryAuditRecorder()
	engine := New(Config{}, recorder)

	decision := engine.Decide(context.Background(), Request{
		RunID:       "run-1",
		AttemptID:   "attempt-1",
		ToolName:    "repo.read",
		Capability:  "repo:read",
		ReadOnly:    true,
		Paths:       []string{"README.md"},
		Timeout:     time.Second,
		OutputLimit: 1024,
	})
	if decision.Allowed {
		t.Fatal("empty policy must default to deny")
	}
	if !errors.Is(decision.Err, ErrDenied) {
		t.Fatalf("decision error = %v, want ErrDenied", decision.Err)
	}
	events := recorder.Events()
	if len(events) != 1 || events[0].Kind != AuditPolicyDenied {
		t.Fatalf("audit events = %#v, want one policy_denied event", events)
	}
}

func TestPolicyFailsClosedWhenAuditCannotPersist(t *testing.T) {
	engine := New(Config{
		Rules: []Rule{{
			Tool: "repo.read", Capability: "repo:read", ReadOnly: true,
			Paths: []string{"."}, MaxTimeout: time.Second, MaxOutputBytes: 1024,
		}},
	}, failingAuditRecorder{})
	decision := engine.Decide(context.Background(), Request{
		ToolName: "repo.read", Capability: "repo:read", ReadOnly: true,
		Paths: []string{"README.md"}, Timeout: time.Second, OutputLimit: 1024,
	})
	if decision.Allowed || !errors.Is(decision.Err, ErrDenied) {
		t.Fatalf("audit failure did not fail closed: %#v", decision)
	}
}

func TestYAMLPolicyAllowsOnlyDeclaredCapabilityPathAndBudget(t *testing.T) {
	config, err := Parse([]byte(`
version: v1
default: deny
environment:
  allow:
    - TEST_LABEL
rules:
  - tool: repo.read
    capability: repo:read
    readOnly: true
    paths:
      - docs/
    network: false
    maxTimeout: 2s
    maxOutputBytes: 2048
`))
	if err != nil {
		t.Fatal(err)
	}
	engine := New(config, NewMemoryAuditRecorder())
	allowed := engine.Decide(context.Background(), Request{
		ToolName:    "repo.read",
		Capability:  "repo:read",
		ReadOnly:    true,
		Paths:       []string{"docs/architecture.md"},
		Timeout:     time.Second,
		OutputLimit: 1024,
		Environment: map[string]string{"TEST_LABEL": "phase4"},
	})
	if !allowed.Allowed {
		t.Fatalf("declared request denied: %v", allowed.Err)
	}

	for name, mutate := range map[string]func(*Request){
		"capability": func(request *Request) { request.Capability = "repo:write" },
		"path":       func(request *Request) { request.Paths = []string{"internal/domain/types.go"} },
		"network":    func(request *Request) { request.Network = true },
		"timeout":    func(request *Request) { request.Timeout = 3 * time.Second },
		"output":     func(request *Request) { request.OutputLimit = 4096 },
		"environment": func(request *Request) {
			request.Environment = map[string]string{"OPENAI_API_KEY": "must-not-leak"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			request := Request{
				ToolName:    "repo.read",
				Capability:  "repo:read",
				ReadOnly:    true,
				Paths:       []string{"docs/architecture.md"},
				Timeout:     time.Second,
				OutputLimit: 1024,
				Environment: map[string]string{"TEST_LABEL": "phase4"},
			}
			mutate(&request)
			if decision := engine.Decide(context.Background(), request); decision.Allowed {
				t.Fatalf("mutated request unexpectedly allowed: %#v", request)
			}
		})
	}
}

func TestPolicyYAMLRejectsUnknownFieldsAndNonDenyDefault(t *testing.T) {
	for name, content := range map[string]string{
		"unknown field": "version: v1\ndefault: deny\nrules: []\nallowEverything: true\n",
		"allow default": "version: v1\ndefault: allow\nrules: []\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(content)); err == nil {
				t.Fatalf("unsafe policy YAML unexpectedly parsed: %s", content)
			}
		})
	}
}

func TestNormalizePathRejectsTraversalAbsoluteAndSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "safe"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "safe", "escape")); err != nil {
		t.Fatal(err)
	}

	for _, candidate := range []string{
		"../secret",
		"safe/../../secret",
		filepath.Join(outside, "secret"),
		"safe/escape/secret",
		".git",
		".GIT/config",
		".agentdock/cache",
		".AgentDock/home/go/env",
	} {
		t.Run(strings.ReplaceAll(candidate, "/", "_"), func(t *testing.T) {
			if _, err := NormalizePath(root, candidate); !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("NormalizePath(%q) error = %v, want ErrUnsafePath", candidate, err)
			}
		})
	}

	resolved, err := NormalizePath(root, "safe/new-file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if resolved != "safe/new-file.txt" {
		t.Fatalf("resolved path = %q", resolved)
	}
}

func TestFileAuditArtifactRedactsEnvironmentValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	recorder, err := NewFileAuditRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close()
	if err := recorder.Record(context.Background(), AuditEvent{
		Kind:        AuditPolicyDenied,
		RunID:       "run-1",
		AttemptID:   "attempt-1",
		ToolName:    "repo.test",
		Reason:      "environment key OPENAI_API_KEY is not allowlisted",
		Environment: []string{"OPENAI_API_KEY"},
	}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "must-not-leak") {
		t.Fatalf("audit artifact leaked an environment value: %s", content)
	}
	if !strings.Contains(string(content), `"environment":["OPENAI_API_KEY"]`) {
		t.Fatalf("audit artifact omitted the rejected key name: %s", content)
	}
	for _, explicit := range []string{
		`"read_only":false`,
		`"network":false`,
		`"timeout_millis":0`,
		`"output_limit_bytes":0`,
		`"exit_code":0`,
		`"output_bytes":0`,
	} {
		if !strings.Contains(string(content), explicit) {
			t.Fatalf("audit artifact omitted explicit zero-value evidence %s: %s", explicit, content)
		}
	}
	artifact, err := recorder.Artifact()
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Type != Phase4AuditArtifactType ||
		!strings.HasPrefix(artifact.Digest, "sha256:") ||
		artifact.Size != int64(len(content)) {
		t.Fatalf("audit Artifact = %#v", artifact)
	}
}

type failingAuditRecorder struct{}

func (failingAuditRecorder) Record(context.Context, AuditEvent) error {
	return errors.New("audit unavailable")
}
