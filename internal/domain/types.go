package domain

// DesiredState records operator intent independently from observed runtime state.
type DesiredState string

const (
	DesiredRunning   DesiredState = "Running"
	DesiredPaused    DesiredState = "Paused"
	DesiredCancelled DesiredState = "Cancelled"
)

// Valid reports whether the desired state is part of the phase-1 contract.
func (state DesiredState) Valid() bool {
	switch state {
	case DesiredRunning, DesiredPaused, DesiredCancelled:
		return true
	default:
		return false
	}
}

// Status is the runtime state derived from an ordered event log.
type Status string

const (
	StatusQueued          Status = "Queued"
	StatusProvisioning    Status = "Provisioning"
	StatusReasoning       Status = "Reasoning"
	StatusActing          Status = "Acting"
	StatusVerifying       Status = "Verifying"
	StatusRepairing       Status = "Repairing"
	StatusWaitingApproval Status = "WaitingApproval"
	StatusPaused          Status = "Paused"
	StatusSucceeded       Status = "Succeeded"
	StatusFailed          Status = "Failed"
	StatusCancelled       Status = "Cancelled"
)

// Terminal reports whether no further state transition is permitted.
func (status Status) Terminal() bool {
	switch status {
	case StatusSucceeded, StatusFailed, StatusCancelled:
		return true
	default:
		return false
	}
}

// Run contains the durable metadata and lifecycle dimensions reconstructed by
// Reduce. Timestamps are event data; the reducer never obtains a clock itself.
type Run struct {
	ID             string       `json:"run_id"`
	ScenarioID     string       `json:"scenario_id"`
	SpecHash       string       `json:"spec_hash"`
	DesiredState   DesiredState `json:"desired_state"`
	ObservedState  Status       `json:"observed_state"`
	CurrentAttempt int          `json:"current_attempt"`
	Version        uint64       `json:"version"`
	CreatedAt      string       `json:"created_at,omitempty"`
	UpdatedAt      string       `json:"updated_at,omitempty"`
}

// Attempt is the phase-1 representation of one execution attempt. Later
// persistence work may store it separately, but its identity is already
// explicit in the domain.
type Attempt struct {
	ID              string `json:"attempt_id"`
	RunID           string `json:"run_id"`
	Number          int    `json:"number"`
	WorkspaceDigest string `json:"workspace_digest,omitempty"`
	Reason          string `json:"reason,omitempty"`
	StartedAt       string `json:"started_at,omitempty"`
	FinishedAt      string `json:"finished_at,omitempty"`
}

// State is a deterministic projection. It intentionally uses only value types
// and contains no clock, random source, network client, callback, or hidden
// process-local authority.
type State struct {
	Exists                bool             `json:"exists"`
	Run                   Run              `json:"run"`
	AttemptID             string           `json:"attempt_id,omitempty"`
	ResumeState           Status           `json:"resume_state,omitempty"`
	PendingActionID       string           `json:"pending_action_id,omitempty"`
	PendingActionType     CommandType      `json:"pending_action_type,omitempty"`
	PendingAttemptID      string           `json:"pending_attempt_id,omitempty"`
	PendingActionScope    IdempotencyScope `json:"pending_action_scope,omitempty"`
	LastCompletedActionID string           `json:"last_completed_action_id,omitempty"`
	LastReceiptID         string           `json:"last_receipt_id,omitempty"`
	ReasoningOutput       string           `json:"reasoning_output,omitempty"`
	ToolName              string           `json:"tool_name,omitempty"`
	ToolArguments         string           `json:"tool_arguments,omitempty"`
	PatchProduced         bool             `json:"patch_produced"`
	VerificationPassed    bool             `json:"verification_passed"`
	FailureReason         string           `json:"failure_reason,omitempty"`
	LastEventType         EventType        `json:"last_event_type,omitempty"`
}

// InitialState is the projection of an empty event log.
func InitialState() State {
	return State{}
}

// EventType identifies a durable fact. The complete frozen vocabulary is
// declared here; Reduce returns ErrUnsupportedEvent for facts owned by later
// phases instead of guessing their semantics.
type EventType string

const CurrentEventPayloadVersion = 1

const (
	EventRunCreated                EventType = "RunCreated"
	EventDesiredStateChanged       EventType = "RunDesiredStateChanged"
	EventLeaseAcquired             EventType = "LeaseAcquired"
	EventLeaseRenewed              EventType = "LeaseRenewed"
	EventLeaseExpired              EventType = "LeaseExpired"
	EventAttemptStarted            EventType = "AttemptStarted"
	EventWorkspaceProvisionPlanned EventType = "WorkspaceProvisionPlanned"
	EventWorkspaceProvisioned      EventType = "WorkspaceProvisioned"
	EventReasoningPlanned          EventType = "ReasoningPlanned"
	EventReasoningCompleted        EventType = "ReasoningCompleted"
	EventToolCallPlanned           EventType = "ToolCallPlanned"
	EventToolCallCompleted         EventType = "ToolCallCompleted"
	EventToolCallFailed            EventType = "ToolCallFailed"
	EventPatchProduced             EventType = "PatchProduced"
	EventVerificationPlanned       EventType = "VerificationPlanned"
	EventVerificationPassed        EventType = "VerificationPassed"
	EventVerificationFailed        EventType = "VerificationFailed"
	EventRepairScheduled           EventType = "RepairScheduled"
	EventApprovalRequested         EventType = "ApprovalRequested"
	EventApprovalResolved          EventType = "ApprovalResolved"
	EventCheckpointSaved           EventType = "CheckpointSaved"
	EventActionPlanned             EventType = "ActionPlanned"
	EventActionCompleted           EventType = "ActionCompleted"
	EventActionFailed              EventType = "ActionFailed"
	EventRunSucceeded              EventType = "RunSucceeded"
	EventRunFailed                 EventType = "RunFailed"
	EventRunCancelled              EventType = "RunCancelled"
)

// EventData is the small, framework-neutral phase-1 payload. It is deliberately
// not a phase-4 Tool Contract or a provider message schema.
type EventData struct {
	ScenarioID       string           `json:"scenario_id,omitempty"`
	SpecHash         string           `json:"spec_hash,omitempty"`
	DesiredState     DesiredState     `json:"desired_state,omitempty"`
	AttemptID        string           `json:"attempt_id,omitempty"`
	ActionID         string           `json:"action_id,omitempty"`
	ActionType       CommandType      `json:"action_type,omitempty"`
	IdempotencyScope IdempotencyScope `json:"idempotency_scope,omitempty"`
	ReceiptID        string           `json:"receipt_id,omitempty"`
	Outcome          ActionOutcome    `json:"outcome,omitempty"`
	Output           string           `json:"output,omitempty"`
	OutputDigest     string           `json:"output_digest,omitempty"`
	ArtifactID       string           `json:"artifact_id,omitempty"`
	ArtifactDigest   string           `json:"artifact_digest,omitempty"`
	ToolName         string           `json:"tool_name,omitempty"`
	ToolArguments    string           `json:"tool_arguments,omitempty"`
	Reason           string           `json:"reason,omitempty"`
}

// Event is the ordered envelope consumed by Reduce. Seq is assigned by the
// EventStore; CreatedAt is supplied by the caller/store and is never generated
// by the reducer.
type Event struct {
	RunID          string    `json:"run_id"`
	Seq            uint64    `json:"seq"`
	Type           EventType `json:"event_type"`
	PayloadVersion int       `json:"payload_version"`
	Data           EventData `json:"payload"`
	IdempotencyKey string    `json:"idempotency_key"`
	CausationID    string    `json:"causation_id,omitempty"`
	CorrelationID  string    `json:"correlation_id,omitempty"`
	WorkerID       string    `json:"worker_id,omitempty"`
	FencingToken   uint64    `json:"fencing_token,omitempty"`
	CreatedAt      string    `json:"created_at,omitempty"`
}

// CommandType is the single next action selected by Decide.
type CommandType string

const (
	CommandNoop               CommandType = "Noop"
	CommandStartAttempt       CommandType = "StartAttempt"
	CommandProvisionWorkspace CommandType = "ProvisionWorkspace"
	CommandRunReasoner        CommandType = "RunReasoner"
	CommandApplyPatch         CommandType = "ApplyPatch"
	CommandVerify             CommandType = "Verify"
	CommandSucceedRun         CommandType = "SucceedRun"
	CommandFailRun            CommandType = "FailRun"
	CommandCancelRun          CommandType = "CancelRun"
)

// IdempotencyScope describes whether a planned action may be retried after an
// ambiguous crash window. It is deliberately narrower than a future Tool
// Contract: phase 3 only needs an honest recovery decision.
type IdempotencyScope string

const (
	IdempotencyScoped IdempotencyScope = "scoped-idempotent"
	IdempotencyUnsafe IdempotencyScope = "unsafe"
)

func (scope IdempotencyScope) Valid() bool {
	return scope == IdempotencyScoped || scope == IdempotencyUnsafe
}

// ActionOutcome distinguishes a known action failure from an outcome that
// cannot be safely inferred after a crash.
type ActionOutcome string

const (
	ActionOutcomeFailed    ActionOutcome = "failed"
	ActionOutcomeAmbiguous ActionOutcome = "ambiguous"
)

// Command describes at most one reconcile action. ActionID is derived only
// from State, so repeated decisions over the same state are identical.
type Command struct {
	Type      CommandType `json:"type"`
	RunID     string      `json:"run_id,omitempty"`
	ActionID  string      `json:"action_id,omitempty"`
	AttemptID string      `json:"attempt_id,omitempty"`
}
