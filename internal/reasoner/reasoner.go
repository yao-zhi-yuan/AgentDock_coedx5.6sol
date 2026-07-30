package reasoner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

const Phase1PatchTool = "phase1.patch"

var (
	ErrIllegalToolCall = errors.New("illegal phase-1 tool call")
	ErrInvalidResult   = errors.New("invalid reasoner result")
)

// Request is the minimum runtime-owned input needed by phase 1.
type Request struct {
	RunID      string
	ScenarioID string
	AttemptID  string
}

// ToolCall is a minimal internal representation. It is not a Tool Contract and
// does not execute anything.
type ToolCall struct {
	Name      string
	Arguments string
}

// Result is framework-neutral and contains no Eino/provider type.
type Result struct {
	Output   string
	ToolCall *ToolCall
}

// Reasoner is the phase-1 seam used by the Controller.
type Reasoner interface {
	Reason(context.Context, Request) (Result, error)
}

// ValidatePhase1Result accepts only the inert fake patch marker used to drive
// the state machine. Schema/capability/policy enforcement belongs to phase 4.
func ValidatePhase1Result(result Result) error {
	if result.ToolCall == nil {
		return fmt.Errorf("%w: missing tool call", ErrInvalidResult)
	}
	if result.ToolCall.Name != Phase1PatchTool {
		return fmt.Errorf("%w: %q", ErrIllegalToolCall, result.ToolCall.Name)
	}
	if result.ToolCall.Arguments == "" || !json.Valid([]byte(result.ToolCall.Arguments)) {
		return fmt.Errorf("%w: tool arguments must be valid JSON", ErrInvalidResult)
	}
	return nil
}
