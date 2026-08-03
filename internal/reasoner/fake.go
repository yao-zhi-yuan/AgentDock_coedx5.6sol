package reasoner

import (
	"context"
	"sync"
)

// FakeReasoner returns deterministic normalized streams and never reads model
// credentials or provider configuration.
type FakeReasoner struct {
	mu          sync.Mutex
	events      []Event
	sourceErr   error
	calls       int
	defaultMode bool
}

func NewFakeReasoner() *FakeReasoner {
	return &FakeReasoner{defaultMode: true, events: cloneEvents([]Event{
		Event{Type: EventTextDelta, Text: "deterministic fake patch"},
		Event{Type: EventToolCall, ToolCall: &ToolCall{
			ID: "phase1-fake-call", Name: Phase1PatchTool, Arguments: `{"patch":"fake"}`,
		}},
		Event{Type: EventUsage, Usage: &Usage{InputTokens: 0, OutputTokens: 1, TotalTokens: 1}},
		Event{Type: EventFinish, Finish: &Finish{Reason: "tool_calls"}},
	})}
}

func NewFakeReasonerWithEvents(events ...Event) *FakeReasoner {
	return &FakeReasoner{events: cloneEvents(events)}
}

// NewFakeReasonerWithResult preserves the phase-1 deterministic test seam.
func NewFakeReasonerWithResult(result Result) *FakeReasoner {
	events := []Event{}
	if result.Output != "" {
		events = append(events, Event{Type: EventTextDelta, Text: result.Output})
	}
	if result.ToolCall != nil {
		call := *result.ToolCall
		if call.ID == "" {
			call.ID = "scripted-fake-call"
		}
		events = append(events, Event{Type: EventToolCall, ToolCall: &call})
	}
	if result.Usage != (Usage{}) {
		events = append(events, Event{Type: EventUsage, Usage: &result.Usage})
	}
	reason := result.FinishReason
	if reason == "" {
		reason = "tool_calls"
	}
	events = append(events, Event{Type: EventFinish, Finish: &Finish{Reason: reason}})
	return NewFakeReasonerWithEvents(events...)
}

func NewFailingFakeReasoner(err error) *FakeReasoner {
	return &FakeReasoner{sourceErr: err}
}

func (fake *FakeReasoner) Reason(ctx context.Context, request Request) Stream {
	fake.mu.Lock()
	fake.calls++
	events := cloneEvents(fake.events)
	sourceErr := fake.sourceErr
	defaultMode := fake.defaultMode
	fake.mu.Unlock()
	if defaultMode && len(request.Tools) > 0 {
		events = []Event{
			{Type: EventTextDelta, Text: "deterministic fake repository inspection"},
			{Type: EventToolCall, ToolCall: &ToolCall{ID: "phase5-fake-call", Name: "repo.read", Arguments: `{"path":"."}`}},
			{Type: EventUsage, Usage: &Usage{InputTokens: 0, OutputTokens: 1, TotalTokens: 1}},
			{Type: EventFinish, Finish: &Finish{Reason: "tool_calls"}},
		}
	}
	if err := ctx.Err(); err != nil {
		sourceErr = err
	}
	return NewNormalizedStream(request, &sliceStream{events: events, err: sourceErr})
}

func (fake *FakeReasoner) CallCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.calls
}
