package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	ErrDestroyed         = errors.New("sandbox is destroyed")
	ErrTimeout           = errors.New("sandbox command timed out")
	ErrCommandNotAllowed = errors.New("sandbox command is not allowed")
)

// Spec binds one disposable workspace to exactly one Run/Attempt.
type Spec struct {
	RunID      string
	AttemptID  string
	Repository string
	Revision   string
}

type Scope struct {
	RunID     string
	AttemptID string
}

// Request is an internal execution request. The tool layer constructs this
// from a validated contract and policy decision; no host shell string exists.
type Request struct {
	Command         []string
	Stdin           []byte
	Environment     map[string]string
	Timeout         time.Duration
	OutputLimit     int
	ToolName        string
	ContractVersion string
}

type Result struct {
	Stdout    []byte
	Stderr    []byte
	ExitCode  int
	TimedOut  bool
	Truncated bool
}

// Sandbox is the phase-4 execution seam. The phase-3 Controller keeps its
// durable ActionExecutor/Receipt semantics; phase 5 may drive this seam only
// through the Tool Contract service.
type Sandbox interface {
	Execute(context.Context, Request) (Result, error)
	Scope() Scope
	Workspace() string
	Destroy(context.Context) error
}

type ExitError struct {
	Code int
}

func (err *ExitError) Error() string {
	return fmt.Sprintf("sandbox command exited with code %d", err.Code)
}
