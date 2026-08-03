package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agentdock/agentdock-verify/internal/policy"
	"github.com/agentdock/agentdock-verify/internal/sandbox"
)

var ErrScopeMismatch = errors.New("tool invocation scope does not match sandbox scope")

type Invocation struct {
	RunID       string
	AttemptID   string
	ToolName    string
	Input       json.RawMessage
	Environment map[string]string
}

type Response struct {
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	ExitCode  int    `json:"exitCode"`
	TimedOut  bool   `json:"timedOut"`
	Truncated bool   `json:"truncated"`
}

type Service struct {
	registry *Registry
	policy   *policy.Engine
	sandbox  sandbox.Sandbox
	audit    policy.AuditRecorder
}

func NewService(
	registry *Registry,
	engine *policy.Engine,
	instance sandbox.Sandbox,
	audit policy.AuditRecorder,
) *Service {
	return &Service{
		registry: registry,
		policy:   engine,
		sandbox:  instance,
		audit:    audit,
	}
}

// Contracts returns the immutable execution-contract snapshot bound to this
// Service. CodingAgent uses it to reject a model-visible contract drift before
// calling Reasoner.
func (service *Service) Contracts() ([]Contract, error) {
	if service == nil || service.registry == nil {
		return nil, errors.New("tool service registry is not configured")
	}
	contracts := service.registry.Contracts()
	if len(contracts) == 0 {
		return nil, errors.New("tool service registry has no contracts")
	}
	return contracts, nil
}

func (service *Service) Invoke(ctx context.Context, invocation Invocation) (Response, error) {
	if service.registry == nil || service.policy == nil || service.sandbox == nil || service.audit == nil {
		return Response{}, errors.New("tool service is not configured")
	}
	scope := service.sandbox.Scope()
	if scope.RunID == "" || scope.AttemptID == "" {
		return Response{}, errors.New("sandbox scope is not configured")
	}
	policyVersion := service.policy.Version()
	if invocation.RunID != scope.RunID || invocation.AttemptID != scope.AttemptID {
		auditErr := policy.RecordBounded(service.audit, policy.AuditEvent{
			Kind:               policy.AuditPolicyDenied,
			RunID:              scope.RunID,
			AttemptID:          scope.AttemptID,
			RequestedRunID:     invocation.RunID,
			RequestedAttemptID: invocation.AttemptID,
			ToolName:           invocation.ToolName,
			PolicyVersion:      policyVersion,
			Reason:             "invocation scope does not match the bound sandbox scope",
		})
		return Response{}, errors.Join(ErrScopeMismatch, auditErr)
	}
	contract, ok := service.registry.Get(invocation.ToolName)
	if !ok {
		auditErr := policy.RecordBounded(service.audit, policy.AuditEvent{
			Kind:          policy.AuditPolicyDenied,
			RunID:         scope.RunID,
			AttemptID:     scope.AttemptID,
			ToolName:      invocation.ToolName,
			PolicyVersion: policyVersion,
			Reason:        "tool has no registered phase-4 contract",
		})
		return Response{}, errors.Join(
			fmt.Errorf("%w: %s", ErrUnknownTool, invocation.ToolName),
			auditErr,
		)
	}
	if err := contract.ValidateInput(invocation.Input); err != nil {
		auditErr := policy.RecordBounded(service.audit, policy.AuditEvent{
			Kind:            policy.AuditPolicyDenied,
			RunID:           scope.RunID,
			AttemptID:       scope.AttemptID,
			ToolName:        invocation.ToolName,
			ContractVersion: contract.Version,
			PolicyVersion:   policyVersion,
			Reason:          "input JSON Schema validation failed",
		})
		return Response{}, errors.Join(err, auditErr)
	}
	command, stdin, paths, err := service.buildRequest(contract, invocation.Input)
	if err != nil {
		auditErr := policy.RecordBounded(service.audit, policy.AuditEvent{
			Kind:            policy.AuditPolicyDenied,
			RunID:           scope.RunID,
			AttemptID:       scope.AttemptID,
			ToolName:        invocation.ToolName,
			ContractVersion: contract.Version,
			PolicyVersion:   policyVersion,
			Reason:          err.Error(),
		})
		return Response{}, errors.Join(err, auditErr)
	}
	for _, candidate := range paths {
		if _, err := policy.NormalizePath(service.sandbox.Workspace(), candidate); err != nil {
			auditErr := policy.RecordBounded(service.audit, policy.AuditEvent{
				Kind:            policy.AuditPolicyDenied,
				RunID:           scope.RunID,
				AttemptID:       scope.AttemptID,
				ToolName:        invocation.ToolName,
				ContractVersion: contract.Version,
				PolicyVersion:   policyVersion,
				AllowedPaths:    paths,
				Reason:          "path normalization or symlink boundary rejected the request",
			})
			return Response{}, errors.Join(err, auditErr)
		}
	}
	if !policy.PathsAllowed(paths, contract.AllowedPaths) {
		auditErr := policy.RecordBounded(service.audit, policy.AuditEvent{
			Kind:            policy.AuditPolicyDenied,
			RunID:           scope.RunID,
			AttemptID:       scope.AttemptID,
			ToolName:        invocation.ToolName,
			ContractVersion: contract.Version,
			PolicyVersion:   policyVersion,
			AllowedPaths:    paths,
			Reason:          "path is outside Tool Contract allowlist",
		})
		return Response{}, errors.Join(
			fmt.Errorf("%w: path is outside Tool Contract allowlist", policy.ErrDenied),
			auditErr,
		)
	}
	decision := service.policy.Decide(ctx, policy.Request{
		RunID:           scope.RunID,
		AttemptID:       scope.AttemptID,
		ToolName:        contract.Name,
		ContractVersion: contract.Version,
		Capability:      contract.Capability,
		ReadOnly:        contract.ReadOnly,
		Paths:           paths,
		Network:         contract.Network,
		Timeout:         contract.Timeout,
		OutputLimit:     contract.OutputLimit,
		Environment:     invocation.Environment,
	})
	if !decision.Allowed {
		return Response{}, decision.Err
	}
	result, executeErr := service.sandbox.Execute(ctx, sandbox.Request{
		Command:         command,
		Stdin:           stdin,
		Environment:     invocation.Environment,
		Timeout:         contract.Timeout,
		OutputLimit:     contract.OutputLimit,
		ToolName:        contract.Name,
		ContractVersion: contract.Version,
	})
	response := Response{
		Stdout:    string(result.Stdout),
		Stderr:    string(result.Stderr),
		ExitCode:  result.ExitCode,
		TimedOut:  result.TimedOut,
		Truncated: result.Truncated,
	}
	payload, marshalErr := json.Marshal(response)
	if marshalErr != nil {
		return Response{}, marshalErr
	}
	if err := contract.ValidateOutput(payload); err != nil {
		auditErr := policy.RecordBounded(service.audit, policy.AuditEvent{
			Kind:            policy.AuditExecutionFailed,
			RunID:           scope.RunID,
			AttemptID:       scope.AttemptID,
			ToolName:        invocation.ToolName,
			ContractVersion: contract.Version,
			PolicyVersion:   policyVersion,
			Reason:          "output JSON Schema validation failed",
		})
		return Response{}, errors.Join(err, auditErr)
	}
	kind := policy.AuditExecutionCompleted
	reason := ""
	if executeErr != nil {
		kind = policy.AuditExecutionFailed
		reason = "sandbox execution failed"
	}
	if auditErr := policy.RecordBounded(service.audit, policy.AuditEvent{
		Kind:            kind,
		RunID:           scope.RunID,
		AttemptID:       scope.AttemptID,
		ToolName:        invocation.ToolName,
		ContractVersion: contract.Version,
		PolicyVersion:   policyVersion,
		Capability:      contract.Capability,
		ReadOnly:        contract.ReadOnly,
		AllowedPaths:    append([]string(nil), paths...),
		Network:         contract.Network,
		TimeoutMillis:   contract.Timeout.Milliseconds(),
		OutputLimit:     contract.OutputLimit,
		ExitCode:        result.ExitCode,
		OutputBytes:     len(result.Stdout) + len(result.Stderr),
		Reason:          reason,
	}); auditErr != nil {
		return response, errors.Join(executeErr, fmt.Errorf("persist Tool execution audit: %w", auditErr))
	}
	return response, executeErr
}

func (service *Service) buildRequest(contract Contract, input json.RawMessage) ([]string, []byte, []string, error) {
	var object map[string]any
	decoderInput, err := decodeObject(input)
	if err != nil {
		return nil, nil, nil, err
	}
	object = decoderInput
	pathValue := func() (string, error) {
		value, ok := object["path"].(string)
		if !ok || value == "" {
			return "", fmt.Errorf("%w: path is required", ErrSchemaValidation)
		}
		return value, nil
	}
	switch contract.Name {
	case "repo.list":
		path, err := pathValue()
		return []string{"agentdock-sandbox-helper", "list", path}, nil, []string{path}, err
	case "repo.read":
		path, err := pathValue()
		if err != nil {
			return nil, nil, nil, err
		}
		command := []string{"agentdock-sandbox-helper", "read", path}
		start, hasStart := object["startLine"].(float64)
		_, hasEnd := object["endLine"].(float64)
		if hasEnd && !hasStart {
			return nil, nil, nil, fmt.Errorf("%w: endLine requires startLine", ErrSchemaValidation)
		}
		if hasStart {
			if end, endOK := object["endLine"].(float64); endOK && end < start {
				return nil, nil, nil, fmt.Errorf("%w: endLine precedes startLine", ErrSchemaValidation)
			}
			command = append(command, fmt.Sprintf("%.0f", start))
			if end, endOK := object["endLine"].(float64); endOK {
				command = append(command, fmt.Sprintf("%.0f", end))
			}
		}
		return command, nil, []string{path}, nil
	case "repo.search":
		path, pathErr := pathValue()
		pattern, ok := object["pattern"].(string)
		if pathErr != nil {
			return nil, nil, nil, pathErr
		}
		if !ok || pattern == "" {
			return nil, nil, nil, fmt.Errorf("%w: pattern is required", ErrSchemaValidation)
		}
		return []string{"agentdock-sandbox-helper", "search", pattern, path}, nil, []string{path}, nil
	case "repo.apply_patch":
		path, pathErr := pathValue()
		if pathErr != nil {
			return nil, nil, nil, pathErr
		}
		oldText, oldOK := object["old"].(string)
		newText, newOK := object["new"].(string)
		if !oldOK || !newOK || oldText == "" || strings.Contains(newText, oldText) {
			return nil, nil, nil, fmt.Errorf(
				"%w: patch new text must not retain old text",
				ErrSchemaValidation,
			)
		}
		return []string{"agentdock-sandbox-helper", "apply-patch"}, append([]byte(nil), input...), []string{path}, nil
	case "repo.test":
		rawPackages, ok := object["packages"].([]any)
		if !ok || len(rawPackages) == 0 {
			return nil, nil, nil, fmt.Errorf("%w: packages are required", ErrSchemaValidation)
		}
		command := []string{"agentdock-sandbox-helper", "test"}
		paths := make([]string, 0, len(rawPackages))
		if run, ok := object["run"].(string); ok && run != "" {
			if strings.HasPrefix(run, "-") || strings.ContainsRune(run, 0) {
				return nil, nil, nil, fmt.Errorf("%w: unsafe test run expression", ErrSchemaValidation)
			}
		}
		for _, raw := range rawPackages {
			name, ok := raw.(string)
			if !ok ||
				(name != "." && !strings.HasPrefix(name, "./")) ||
				strings.HasPrefix(name, "-") ||
				filepath.IsAbs(name) {
				return nil, nil, nil, fmt.Errorf("%w: unsafe test package", ErrSchemaValidation)
			}
			pathCandidate := strings.TrimSuffix(name, "/...")
			if pathCandidate == "" {
				pathCandidate = "."
			}
			if _, err := policy.NormalizePath(service.sandbox.Workspace(), pathCandidate); err != nil {
				return nil, nil, nil, err
			}
			paths = append(paths, pathCandidate)
		}
		sort.Strings(paths)
		return command, append([]byte(nil), input...), paths, nil
	default:
		return nil, nil, nil, fmt.Errorf("%w: %s", ErrUnknownTool, contract.Name)
	}
}

func decodeObject(input json.RawMessage) (map[string]any, error) {
	var object map[string]any
	if err := json.Unmarshal(input, &object); err != nil {
		return nil, err
	}
	return object, nil
}
