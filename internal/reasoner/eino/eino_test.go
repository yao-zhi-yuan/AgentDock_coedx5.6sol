package eino

import (
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/agentdock/agentdock-verify/internal/reasoner"
	"github.com/agentdock/agentdock-verify/internal/tools"
	"github.com/cloudwego/eino/schema"
)

func TestEinoToolCallAtomicFieldsRepeatOrConflict(t *testing.T) {
	index := 0
	for name, chunks := range map[string][]*schema.Message{
		"identical repeat": {
			{ToolCalls: []schema.ToolCall{{Index: &index, ID: "call-1", Function: schema.FunctionCall{Name: "repo.read", Arguments: `{"path":"`}}}},
			{ToolCalls: []schema.ToolCall{{Index: &index, ID: "call-1", Function: schema.FunctionCall{Name: "repo.read", Arguments: `README.md"}`}}}, ResponseMeta: &schema.ResponseMeta{FinishReason: "tool_calls", Usage: &schema.TokenUsage{TotalTokens: 1}}},
		},
		"conflicting id": {
			{ToolCalls: []schema.ToolCall{{Index: &index, ID: "call-1", Function: schema.FunctionCall{Name: "repo.read", Arguments: `{}`}}}},
			{ToolCalls: []schema.ToolCall{{Index: &index, ID: "call-2", Function: schema.FunctionCall{Name: "repo.read"}}}},
		},
		"conflicting name": {
			{ToolCalls: []schema.ToolCall{{Index: &index, ID: "call-1", Function: schema.FunctionCall{Name: "repo.read", Arguments: `{}`}}}},
			{ToolCalls: []schema.ToolCall{{Index: &index, Function: schema.FunctionCall{Name: "repo.test"}}}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			stream := newEinoStream(schema.StreamReaderFromArray(chunks))
			var events []reasoner.Event
			for {
				event, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					if name == "identical repeat" {
						t.Fatalf("Recv() error = %v", err)
					}
					return
				}
				events = append(events, event)
			}
			if name != "identical repeat" {
				t.Fatal("conflicting atomic Tool Call field was accepted")
			}
			if len(events) != 3 || events[0].ToolCall == nil || events[0].ToolCall.Name != "repo.read" || events[0].ToolCall.Arguments != `{"path":"README.md"}` {
				t.Fatalf("events = %#v", events)
			}
		})
	}
}

func TestEinoToolContractPreservesCompleteJSONSchema(t *testing.T) {
	contract := tools.Contract{
		Name: "repo.custom", Version: "v1", Capability: "repo:read", ReadOnly: true,
		Timeout: 1, OutputLimit: 1, AllowedPaths: []string{"."}, Idempotency: "read only",
		InputSchema:  json.RawMessage(`{"type":"object","additionalProperties":false,"required":["path","count","items"],"properties":{"path":{"type":"string","minLength":2},"count":{"type":"integer","minimum":1},"items":{"type":"array","minItems":1,"items":{"type":"string"}}}}`),
		OutputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{}}`),
	}
	infos, err := toEinoTools([]tools.Contract{contract})
	if err != nil {
		t.Fatal(err)
	}
	got, err := infos[0].ParamsOneOf.ToJSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{}
	gotMap := map[string]any{}
	if err := json.Unmarshal(contract.InputSchema, &want); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &gotMap); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotMap, want) {
		t.Fatalf("Eino schema = %s, want exact %s", encoded, contract.InputSchema)
	}
}
