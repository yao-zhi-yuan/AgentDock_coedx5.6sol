package reasoner

import (
	"context"
	"sync"
)

// FakeReasoner returns one deterministic scripted result and records calls in a
// race-safe counter.
type FakeReasoner struct {
	mu     sync.Mutex
	result Result
	err    error
	calls  int
}

// NewFakeReasoner creates the successful Gate-1 fake.
func NewFakeReasoner() *FakeReasoner {
	return NewFakeReasonerWithResult(Result{
		Output: "deterministic fake patch",
		ToolCall: &ToolCall{
			Name:      Phase1PatchTool,
			Arguments: `{"patch":"fake"}`,
		},
	})
}

// NewFakeReasonerWithResult scripts a result, including negative-test values.
func NewFakeReasonerWithResult(result Result) *FakeReasoner {
	return &FakeReasoner{result: cloneResult(result)}
}

// NewFailingFakeReasoner scripts a framework-neutral reasoning error.
func NewFailingFakeReasoner(err error) *FakeReasoner {
	return &FakeReasoner{err: err}
}

// Reason implements Reasoner.
func (fake *FakeReasoner) Reason(ctx context.Context, _ Request) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls++
	return cloneResult(fake.result), fake.err
}

// CallCount returns the number of calls made.
func (fake *FakeReasoner) CallCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.calls
}

func cloneResult(result Result) Result {
	cloned := result
	if result.ToolCall != nil {
		toolCall := *result.ToolCall
		cloned.ToolCall = &toolCall
	}
	return cloned
}
