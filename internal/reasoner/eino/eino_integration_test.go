//go:build integration

package eino_test

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/agentdock/agentdock-verify/internal/reasoner"
	einoreasoner "github.com/agentdock/agentdock-verify/internal/reasoner/eino"
	"github.com/agentdock/agentdock-verify/internal/tools"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type scriptedModel struct {
	input  []*schema.Message
	stream func() (*schema.StreamReader[*schema.Message], error)
}

type statusError int

func (failure statusError) Error() string   { return "provider stream failed with secret details" }
func (failure statusError) StatusCode() int { return int(failure) }

func (modelStub *scriptedModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return nil, errors.New("Generate must not be used by streaming EinoReasoner")
}

func (modelStub *scriptedModel) Stream(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	modelStub.input = input
	return modelStub.stream()
}

func TestEinoReasonerConvertsMessagesStreamingToolUsageAndFinish(t *testing.T) {
	index := 0
	modelStub := &scriptedModel{stream: func() (*schema.StreamReader[*schema.Message], error) {
		return schema.StreamReaderFromArray([]*schema.Message{
			{Role: schema.Assistant, Content: "hel"},
			{Role: schema.Assistant, Content: "lo", ToolCalls: []schema.ToolCall{{
				Index: &index, ID: "call-1", Type: "function",
				Function: schema.FunctionCall{Name: "repo.read", Arguments: `{"path":"internal/`},
			}}},
			{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
				Index:    &index,
				Function: schema.FunctionCall{Name: "repo.read", Arguments: `user/name.go"}`},
			}}},
			{Role: schema.Assistant, ResponseMeta: &schema.ResponseMeta{
				FinishReason: "tool_calls",
				Usage:        &schema.TokenUsage{PromptTokens: 12, CompletionTokens: 8, TotalTokens: 20},
			}},
		}), nil
	}}
	adapter, err := einoreasoner.New(modelStub)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	request := phase5Request(t, 100)
	request.Messages = []reasoner.Message{
		{Role: reasoner.RoleUser, Content: "fix"},
		{Role: reasoner.RoleTool, Content: "read output", ToolCallID: "previous", ToolName: "repo.read"},
	}
	events := collectEvents(t, adapter.Reason(context.Background(), request))
	want := []reasoner.Event{
		{Type: reasoner.EventTextDelta, Text: "hel"},
		{Type: reasoner.EventTextDelta, Text: "lo"},
		{Type: reasoner.EventToolCall, ToolCall: &reasoner.ToolCall{ID: "call-1", Name: "repo.read", Arguments: `{"path":"internal/user/name.go"}`}},
		{Type: reasoner.EventUsage, Usage: &reasoner.Usage{InputTokens: 12, OutputTokens: 8, TotalTokens: 20}},
		{Type: reasoner.EventFinish, Finish: &reasoner.Finish{Reason: "tool_calls"}},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("normalized events = %#v, want %#v", events, want)
	}
	if len(modelStub.input) != 4 ||
		modelStub.input[0].Role != schema.System ||
		modelStub.input[0].Content != reasoner.CodingAgentSystemContract ||
		modelStub.input[2].ToolCallID != "previous" ||
		modelStub.input[3].Role != schema.User {
		t.Fatalf("Eino input conversion = %#v", modelStub.input)
	}
}

func TestEinoStreamingInterruptionBecomesRecoverableError(t *testing.T) {
	modelStub := &scriptedModel{stream: func() (*schema.StreamReader[*schema.Message], error) {
		reader, writer := schema.Pipe[*schema.Message](2)
		writer.Send(&schema.Message{Role: schema.Assistant, Content: "partial"}, nil)
		writer.Send(nil, errors.New("provider connection reset"))
		writer.Close()
		return reader, nil
	}}
	adapter, err := einoreasoner.New(modelStub)
	if err != nil {
		t.Fatal(err)
	}
	events := collectEvents(t, adapter.Reason(context.Background(), phase5Request(t, 100)))
	if len(events) != 2 || events[0].Text != "partial" || events[1].Error == nil {
		t.Fatalf("interrupted events = %#v", events)
	}
	if events[1].Error.Class != reasoner.ErrorStreamingInterrupted || !events[1].Error.Retryable {
		t.Fatalf("stream error = %#v, want recoverable interruption", events[1].Error)
	}
}

func TestEinoAsyncProviderAuthenticationAndRateErrorsStayClassified(t *testing.T) {
	for name, code := range map[string]int{"authentication": 401, "rate limit": 429} {
		t.Run(name, func(t *testing.T) {
			modelStub := &scriptedModel{stream: func() (*schema.StreamReader[*schema.Message], error) {
				reader, writer := schema.Pipe[*schema.Message](1)
				writer.Send(nil, statusError(code))
				writer.Close()
				return reader, nil
			}}
			adapter, err := einoreasoner.New(modelStub)
			if err != nil {
				t.Fatal(err)
			}
			events := collectEvents(t, adapter.Reason(context.Background(), phase5Request(t, 100)))
			if len(events) != 1 || events[0].Error == nil {
				t.Fatalf("events = %#v, want one classified Error", events)
			}
			want := reasoner.ErrorProviderAuthentication
			if code == 429 {
				want = reasoner.ErrorProviderRateLimited
			}
			if events[0].Error.Class != want || events[0].Error.Message == statusError(code).Error() {
				t.Fatalf("classified Error = %#v, want class %q and sanitized message", events[0].Error, want)
			}
		})
	}
}

func TestEinoUsageComputesMissingProviderTotal(t *testing.T) {
	modelStub := &scriptedModel{stream: func() (*schema.StreamReader[*schema.Message], error) {
		return schema.StreamReaderFromArray([]*schema.Message{{
			Role: schema.Assistant,
			ResponseMeta: &schema.ResponseMeta{
				FinishReason: "stop",
				Usage:        &schema.TokenUsage{PromptTokens: 3, CompletionTokens: 2},
			},
		}}), nil
	}}
	adapter, err := einoreasoner.New(modelStub)
	if err != nil {
		t.Fatal(err)
	}
	events := collectEvents(t, adapter.Reason(context.Background(), phase5Request(t, 100)))
	if len(events) != 2 || events[0].Usage == nil || events[0].Usage.TotalTokens != 5 {
		t.Fatalf("usage normalization = %#v", events)
	}
}

func collectEvents(t *testing.T, stream reasoner.Stream) []reasoner.Event {
	t.Helper()
	defer stream.Close()
	var events []reasoner.Event
	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return events
		}
		if err != nil {
			t.Fatalf("Recv() error = %v", err)
		}
		events = append(events, event)
	}
}

func phase5Request(t *testing.T, tokenLimit int) reasoner.Request {
	t.Helper()
	registry, err := tools.NewBuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	contracts := make([]tools.Contract, 0, len(registry.Names()))
	for _, name := range registry.Names() {
		contract, ok := registry.Get(name)
		if !ok {
			t.Fatalf("registry lost Tool %q", name)
		}
		contracts = append(contracts, contract)
	}
	return reasoner.Request{
		Messages:    []reasoner.Message{{Role: reasoner.RoleUser, Content: "fix"}},
		Tools:       contracts,
		TaskSummary: "fix the bug",
		Budget:      reasoner.Budget{TokenLimit: tokenLimit},
	}
}
