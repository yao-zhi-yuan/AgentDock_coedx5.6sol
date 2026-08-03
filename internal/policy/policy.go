package policy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

var (
	ErrDenied     = errors.New("policy denied")
	ErrUnsafePath = errors.New("unsafe repository path")
)

type EnvironmentPolicy struct {
	Allow []string `yaml:"allow"`
}

type Rule struct {
	Tool           string        `yaml:"tool"`
	Capability     string        `yaml:"capability"`
	ReadOnly       bool          `yaml:"readOnly"`
	Paths          []string      `yaml:"paths"`
	Network        bool          `yaml:"network"`
	MaxTimeout     time.Duration `yaml:"-"`
	MaxOutputBytes int           `yaml:"maxOutputBytes"`
}

type Config struct {
	Version     string            `yaml:"version"`
	Default     string            `yaml:"default"`
	Environment EnvironmentPolicy `yaml:"environment"`
	Rules       []Rule            `yaml:"rules"`
}

type rawRule struct {
	Tool           string   `yaml:"tool"`
	Capability     string   `yaml:"capability"`
	ReadOnly       bool     `yaml:"readOnly"`
	Paths          []string `yaml:"paths"`
	Network        bool     `yaml:"network"`
	MaxTimeout     string   `yaml:"maxTimeout"`
	MaxOutputBytes int      `yaml:"maxOutputBytes"`
}

type rawConfig struct {
	Version     string            `yaml:"version"`
	Default     string            `yaml:"default"`
	Environment EnvironmentPolicy `yaml:"environment"`
	Rules       []rawRule         `yaml:"rules"`
}

func Parse(content []byte) (Config, error) {
	var raw rawConfig
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	if err := decoder.Decode(&raw); err != nil {
		return Config{}, fmt.Errorf("parse policy YAML: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Config{}, errors.New("policy YAML must contain exactly one document")
		}
		return Config{}, fmt.Errorf("parse trailing policy YAML: %w", err)
	}
	if raw.Version == "" || raw.Default != "deny" {
		return Config{}, errors.New("policy version is required and default must be deny")
	}
	config := Config{
		Version:     raw.Version,
		Default:     raw.Default,
		Environment: raw.Environment,
	}
	for _, candidate := range raw.Rules {
		timeout, err := time.ParseDuration(candidate.MaxTimeout)
		if err != nil || timeout <= 0 {
			return Config{}, fmt.Errorf("rule %q has invalid maxTimeout %q", candidate.Tool, candidate.MaxTimeout)
		}
		if candidate.Tool == "" ||
			candidate.Capability == "" ||
			len(candidate.Paths) == 0 ||
			candidate.MaxOutputBytes <= 0 {
			return Config{}, fmt.Errorf("rule %q is incomplete", candidate.Tool)
		}
		config.Rules = append(config.Rules, Rule{
			Tool:           candidate.Tool,
			Capability:     candidate.Capability,
			ReadOnly:       candidate.ReadOnly,
			Paths:          append([]string(nil), candidate.Paths...),
			Network:        candidate.Network,
			MaxTimeout:     timeout,
			MaxOutputBytes: candidate.MaxOutputBytes,
		})
	}
	return config, nil
}

type Request struct {
	RunID           string
	AttemptID       string
	ToolName        string
	ContractVersion string
	Capability      string
	ReadOnly        bool
	Paths           []string
	Network         bool
	Timeout         time.Duration
	OutputLimit     int
	Environment     map[string]string
}

type Decision struct {
	Allowed bool
	Err     error
}

type Engine struct {
	config Config
	audit  AuditRecorder
}

func New(config Config, recorder AuditRecorder) *Engine {
	return &Engine{config: config, audit: recorderOrUnavailable(recorder)}
}

func (engine *Engine) Version() string {
	return engine.config.Version
}

func (engine *Engine) Decide(ctx context.Context, request Request) Decision {
	reason := "no matching allow rule"
	matchedTool := false
	for index := range engine.config.Rules {
		rule := &engine.config.Rules[index]
		if rule.Tool != request.ToolName {
			continue
		}
		matchedTool = true
		switch {
		case rule.Capability != request.Capability:
			reason = "capability is not allowed"
		case rule.ReadOnly != request.ReadOnly:
			reason = "read-only property does not match policy"
		case request.Network && !rule.Network:
			reason = "network is denied"
		case request.Timeout <= 0 || request.Timeout > rule.MaxTimeout:
			reason = "timeout exceeds policy"
		case request.OutputLimit <= 0 || request.OutputLimit > rule.MaxOutputBytes:
			reason = "output limit exceeds policy"
		case !PathsAllowed(request.Paths, rule.Paths):
			reason = "path is outside policy allowlist"
		case !environmentAllowed(request.Environment, engine.config.Environment.Allow):
			reason = "environment key is not allowlisted"
		default:
			event := auditEventForRequest(AuditPolicyAllowed, engine.config.Version, request, "")
			if err := RecordBounded(engine.audit, event); err != nil {
				return Decision{Err: fmt.Errorf("%w: persist allow audit: %v", ErrDenied, err)}
			}
			return Decision{Allowed: true}
		}
	}
	if !matchedTool {
		reason = "no matching allow rule"
	}
	event := auditEventForRequest(AuditPolicyDenied, engine.config.Version, request, reason)
	if err := RecordBounded(engine.audit, event); err != nil {
		return Decision{Err: fmt.Errorf("%w: %s; persist denial audit: %v", ErrDenied, reason, err)}
	}
	return Decision{Err: fmt.Errorf("%w: %s", ErrDenied, reason)}
}

func auditEventForRequest(kind AuditKind, policyVersion string, request Request, reason string) AuditEvent {
	keys := make([]string, 0, len(request.Environment))
	for key := range request.Environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return AuditEvent{
		Kind:            kind,
		RunID:           request.RunID,
		AttemptID:       request.AttemptID,
		ToolName:        request.ToolName,
		ContractVersion: request.ContractVersion,
		PolicyVersion:   policyVersion,
		Capability:      request.Capability,
		ReadOnly:        request.ReadOnly,
		AllowedPaths:    append([]string(nil), request.Paths...),
		Network:         request.Network,
		Environment:     keys,
		TimeoutMillis:   request.Timeout.Milliseconds(),
		OutputLimit:     request.OutputLimit,
		Reason:          reason,
	}
}

func environmentAllowed(environment map[string]string, allowed []string) bool {
	allowset := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowset[key] = struct{}{}
	}
	for key := range environment {
		if _, ok := allowset[key]; !ok {
			return false
		}
	}
	return true
}

func PathsAllowed(paths []string, allowed []string) bool {
	if len(paths) == 0 {
		return false
	}
	for _, candidate := range paths {
		normalized, err := normalizeLogicalPath(candidate)
		if err != nil {
			return false
		}
		matched := false
		for _, prefix := range allowed {
			normalizedPrefix, prefixErr := normalizeLogicalPath(prefix)
			if prefixErr != nil {
				continue
			}
			if normalizedPrefix == "." ||
				normalized == normalizedPrefix ||
				strings.HasPrefix(normalized, normalizedPrefix+"/") {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func normalizeLogicalPath(candidate string) (string, error) {
	if candidate == "" ||
		filepath.IsAbs(candidate) ||
		filepath.VolumeName(candidate) != "" ||
		strings.ContainsRune(candidate, 0) {
		return "", ErrUnsafePath
	}
	clean := filepath.Clean(filepath.FromSlash(candidate))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", ErrUnsafePath
	}
	slashed := filepath.ToSlash(clean)
	first, _, _ := strings.Cut(slashed, "/")
	if strings.EqualFold(first, ".git") ||
		strings.EqualFold(first, ".agentdock") {
		return "", ErrUnsafePath
	}
	return slashed, nil
}
