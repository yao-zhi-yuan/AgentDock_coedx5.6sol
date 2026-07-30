package domain_test

import (
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"reflect"
	"strconv"
	"testing"

	"github.com/agentdock/agentdock-verify/internal/domain"
)

const testRunID = "run-001"

func TestReduceEmptyEventsReturnsInitialState(t *testing.T) {
	got, err := domain.Reduce(nil)
	if err != nil {
		t.Fatalf("Reduce(nil) error = %v", err)
	}

	want := domain.InitialState()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Reduce(nil) = %#v, want %#v", got, want)
	}
	if got.Exists {
		t.Fatal("empty event log must not manufacture an existing Run")
	}
}

func TestReduceSameEventsIsByteForByteDeterministic(t *testing.T) {
	events := successfulEvents()

	first, err := domain.Reduce(events)
	if err != nil {
		t.Fatalf("first Reduce() error = %v", err)
	}
	second, err := domain.Reduce(events)
	if err != nil {
		t.Fatalf("second Reduce() error = %v", err)
	}

	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal first state: %v", err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("marshal second state: %v", err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("reduction differs:\nfirst:  %s\nsecond: %s", firstJSON, secondJSON)
	}

	if !first.Exists ||
		first.Run.ID != testRunID ||
		first.Run.ScenarioID != "scenario-001" ||
		first.Run.SpecHash != "spec-sha256" ||
		first.Run.DesiredState != domain.DesiredRunning ||
		first.Run.ObservedState != domain.StatusSucceeded ||
		first.Run.CurrentAttempt != 1 ||
		first.Run.Version != uint64(len(events)) ||
		first.Run.CreatedAt != "2026-07-30T10:00:00Z" ||
		first.Run.UpdatedAt != "2026-07-30T10:00:08Z" ||
		first.AttemptID != "attempt-001" ||
		!first.VerificationPassed ||
		first.PendingActionID != "" ||
		first.LastEventType != domain.EventRunSucceeded {
		t.Fatalf("successful state lost an intermediate invariant: %#v", first)
	}
}

func TestReducerSourceHasNoNonDeterministicImports(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "reducer.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse reducer.go: %v", err)
	}
	forbidden := map[string]bool{
		"time":         true,
		"math/rand":    true,
		"math/rand/v2": true,
		"crypto/rand":  true,
		"net":          true,
		"net/http":     true,
		"os":           true,
	}
	for _, spec := range file.Imports {
		path, unquoteErr := strconv.Unquote(spec.Path.Value)
		if unquoteErr != nil {
			t.Fatalf("unquote import %s: %v", spec.Path.Value, unquoteErr)
		}
		if forbidden[path] {
			t.Fatalf("reducer.go imports non-deterministic package %q", path)
		}
	}
}

func TestReduceRejectsMalformedEventSequences(t *testing.T) {
	tests := []struct {
		name   string
		events []domain.Event
		target error
	}{
		{
			name:   "first event sequence is not one",
			events: []domain.Event{event(2, domain.EventRunCreated, "created", domain.EventData{})},
			target: domain.ErrInvalidSequence,
		},
		{
			name: "sequence gap",
			events: []domain.Event{
				event(1, domain.EventRunCreated, "created", domain.EventData{}),
				event(3, domain.EventAttemptStarted, "attempt", domain.EventData{AttemptID: "attempt-001"}),
			},
			target: domain.ErrInvalidSequence,
		},
		{
			name: "out of order",
			events: []domain.Event{
				event(1, domain.EventRunCreated, "created", domain.EventData{}),
				event(3, domain.EventAttemptStarted, "attempt", domain.EventData{AttemptID: "attempt-001"}),
				event(2, domain.EventWorkspaceProvisioned, "workspace", domain.EventData{}),
			},
			target: domain.ErrInvalidSequence,
		},
		{
			name: "duplicate idempotency key",
			events: []domain.Event{
				event(1, domain.EventRunCreated, "same-key", domain.EventData{}),
				event(2, domain.EventAttemptStarted, "same-key", domain.EventData{AttemptID: "attempt-001"}),
			},
			target: domain.ErrDuplicateIdempotencyKey,
		},
		{
			name:   "missing RunCreated",
			events: []domain.Event{event(1, domain.EventAttemptStarted, "attempt", domain.EventData{AttemptID: "attempt-001"})},
			target: domain.ErrInvalidTransition,
		},
		{
			name: "unknown payload version",
			events: []domain.Event{{
				RunID:          testRunID,
				Seq:            1,
				Type:           domain.EventRunCreated,
				PayloadVersion: 2,
				IdempotencyKey: "created",
			}},
			target: domain.ErrInvalidEvent,
		},
		{
			name: "work event while paused",
			events: []domain.Event{
				event(1, domain.EventRunCreated, "created", domain.EventData{}),
				event(2, domain.EventDesiredStateChanged, "pause", domain.EventData{DesiredState: domain.DesiredPaused}),
				event(3, domain.EventAttemptStarted, "attempt", domain.EventData{AttemptID: "attempt-001"}),
			},
			target: domain.ErrInvalidTransition,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := domain.Reduce(tt.events)
			if !errors.Is(err, tt.target) {
				t.Fatalf("Reduce() error = %v, want errors.Is(_, %v)", err, tt.target)
			}
		})
	}
}

func TestReduceResumeRestoresTheInterruptedPath(t *testing.T) {
	events := []domain.Event{
		event(1, domain.EventRunCreated, "created", domain.EventData{}),
		event(2, domain.EventAttemptStarted, "attempt", domain.EventData{AttemptID: "attempt-001"}),
		event(3, domain.EventWorkspaceProvisioned, "workspace", domain.EventData{}),
		event(4, domain.EventDesiredStateChanged, "pause", domain.EventData{DesiredState: domain.DesiredPaused}),
		event(5, domain.EventDesiredStateChanged, "resume", domain.EventData{DesiredState: domain.DesiredRunning}),
	}

	got, err := domain.Reduce(events)
	if err != nil {
		t.Fatalf("Reduce() error = %v", err)
	}
	if got.Run.ObservedState != domain.StatusReasoning {
		t.Fatalf("resume observed state = %s, want %s", got.Run.ObservedState, domain.StatusReasoning)
	}
	if got.ResumeState != "" {
		t.Fatalf("resume marker was not cleared: %q", got.ResumeState)
	}
}

func TestReduceAcceptsPlannedActionResultsWhilePaused(t *testing.T) {
	tests := []struct {
		name        string
		events      []domain.Event
		resumeState domain.Status
		assert      func(*testing.T, domain.State)
	}{
		{
			name: "ReasoningCompleted advances resume target",
			events: []domain.Event{
				event(1, domain.EventRunCreated, "created", domain.EventData{}),
				event(2, domain.EventAttemptStarted, "attempt", domain.EventData{AttemptID: "attempt-001"}),
				event(3, domain.EventWorkspaceProvisioned, "workspace", domain.EventData{}),
				event(4, domain.EventReasoningPlanned, "reason-plan", domain.EventData{ActionID: "action-reason"}),
				event(5, domain.EventDesiredStateChanged, "pause", domain.EventData{DesiredState: domain.DesiredPaused}),
				event(6, domain.EventReasoningCompleted, "reason-done", domain.EventData{
					ActionID:      "action-reason",
					ToolName:      "phase1.patch",
					ToolArguments: `{"patch":"fake"}`,
				}),
			},
			resumeState: domain.StatusActing,
			assert: func(t *testing.T, state domain.State) {
				t.Helper()
				if state.ToolName != "phase1.patch" {
					t.Fatalf("tool name = %q, want phase1.patch", state.ToolName)
				}
			},
		},
		{
			name: "ToolCallFailed remains auditable",
			events: []domain.Event{
				event(1, domain.EventRunCreated, "created", domain.EventData{}),
				event(2, domain.EventAttemptStarted, "attempt", domain.EventData{AttemptID: "attempt-001"}),
				event(3, domain.EventWorkspaceProvisioned, "workspace", domain.EventData{}),
				event(4, domain.EventReasoningPlanned, "reason-plan", domain.EventData{ActionID: "action-reason"}),
				event(5, domain.EventDesiredStateChanged, "pause", domain.EventData{DesiredState: domain.DesiredPaused}),
				event(6, domain.EventToolCallFailed, "tool-failed", domain.EventData{
					ActionID: "action-reason",
					Reason:   "illegal tool",
				}),
			},
			resumeState: domain.StatusReasoning,
			assert: func(t *testing.T, state domain.State) {
				t.Helper()
				if state.FailureReason != "illegal tool" {
					t.Fatalf("failure reason = %q, want illegal tool", state.FailureReason)
				}
			},
		},
		{
			name: "VerificationPassed records evidence",
			events: []domain.Event{
				event(1, domain.EventRunCreated, "created", domain.EventData{}),
				event(2, domain.EventAttemptStarted, "attempt", domain.EventData{AttemptID: "attempt-001"}),
				event(3, domain.EventWorkspaceProvisioned, "workspace", domain.EventData{}),
				event(4, domain.EventReasoningPlanned, "reason-plan", domain.EventData{ActionID: "action-reason"}),
				event(5, domain.EventReasoningCompleted, "reason-done", domain.EventData{
					ActionID:      "action-reason",
					ToolName:      "phase1.patch",
					ToolArguments: `{"patch":"fake"}`,
				}),
				event(6, domain.EventPatchProduced, "patch", domain.EventData{ActionID: "action-patch"}),
				event(7, domain.EventVerificationPlanned, "verify-plan", domain.EventData{ActionID: "action-verify"}),
				event(8, domain.EventDesiredStateChanged, "pause", domain.EventData{DesiredState: domain.DesiredPaused}),
				event(9, domain.EventVerificationPassed, "verify-pass", domain.EventData{ActionID: "action-verify"}),
			},
			resumeState: domain.StatusVerifying,
			assert: func(t *testing.T, state domain.State) {
				t.Helper()
				if !state.VerificationPassed {
					t.Fatal("paused verification result was not recorded")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, err := domain.Reduce(tt.events)
			if err != nil {
				t.Fatalf("Reduce() error = %v", err)
			}
			if state.Run.DesiredState != domain.DesiredPaused ||
				state.Run.ObservedState != domain.StatusPaused ||
				state.ResumeState != tt.resumeState ||
				state.PendingActionID != "" {
				t.Fatalf("paused result state = %#v, want Paused resume=%s with no pending action", state, tt.resumeState)
			}
			if got := domain.Decide(state); got.Type != domain.CommandNoop {
				t.Fatalf("Decide(paused result) = %s, want Noop", got.Type)
			}
			tt.assert(t, state)
		})
	}
}

func TestTerminalStateCannotAdvance(t *testing.T) {
	events := successfulEvents()
	events = append(events, event(uint64(len(events)+1), domain.EventPatchProduced, "late-patch", domain.EventData{}))

	_, err := domain.Reduce(events)
	if !errors.Is(err, domain.ErrTerminalState) {
		t.Fatalf("Reduce() error = %v, want terminal-state rejection", err)
	}
}

func TestValidateTransitionRejectsSucceededToActing(t *testing.T) {
	err := domain.ValidateTransition(domain.StatusSucceeded, domain.StatusActing)
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("ValidateTransition(Succeeded, Acting) error = %v", err)
	}
}

func successfulEvents() []domain.Event {
	return []domain.Event{
		eventAt(1, domain.EventRunCreated, "created", domain.EventData{ScenarioID: "scenario-001", SpecHash: "spec-sha256"}, "2026-07-30T10:00:00Z"),
		eventAt(2, domain.EventAttemptStarted, "attempt", domain.EventData{AttemptID: "attempt-001"}, "2026-07-30T10:00:01Z"),
		eventAt(3, domain.EventWorkspaceProvisioned, "workspace", domain.EventData{}, "2026-07-30T10:00:02Z"),
		eventAt(4, domain.EventReasoningPlanned, "reason-plan", domain.EventData{ActionID: "action-reason"}, "2026-07-30T10:00:03Z"),
		eventAt(5, domain.EventReasoningCompleted, "reason-done", domain.EventData{ActionID: "action-reason", ToolName: "phase1.patch", ToolArguments: `{"patch":"fake"}`}, "2026-07-30T10:00:04Z"),
		eventAt(6, domain.EventPatchProduced, "patch", domain.EventData{ActionID: "action-patch"}, "2026-07-30T10:00:05Z"),
		eventAt(7, domain.EventVerificationPlanned, "verify-plan", domain.EventData{ActionID: "action-verify"}, "2026-07-30T10:00:06Z"),
		eventAt(8, domain.EventVerificationPassed, "verify-pass", domain.EventData{ActionID: "action-verify"}, "2026-07-30T10:00:07Z"),
		eventAt(9, domain.EventRunSucceeded, "succeeded", domain.EventData{}, "2026-07-30T10:00:08Z"),
	}
}

func event(seq uint64, eventType domain.EventType, key string, data domain.EventData) domain.Event {
	return eventAt(seq, eventType, key, data, "")
}

func eventAt(seq uint64, eventType domain.EventType, key string, data domain.EventData, createdAt string) domain.Event {
	return domain.Event{
		RunID:          testRunID,
		Seq:            seq,
		Type:           eventType,
		PayloadVersion: domain.CurrentEventPayloadVersion,
		Data:           data,
		IdempotencyKey: key,
		CreatedAt:      createdAt,
	}
}
