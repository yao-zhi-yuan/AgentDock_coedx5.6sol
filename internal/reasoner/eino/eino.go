// Package eino confines every CloudWeGo Eino type to the phase-5 adapter
// boundary. Controller and durable runtime packages import only reasoner.
package eino

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/agentdock/agentdock-verify/internal/reasoner"
	"github.com/agentdock/agentdock-verify/internal/tools"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
)

type EinoReasoner struct {
	model model.BaseChatModel
}

func New(chatModel model.BaseChatModel) (*EinoReasoner, error) {
	if chatModel == nil {
		return nil, errors.New("Eino ChatModel is required")
	}
	return &EinoReasoner{model: chatModel}, nil
}

func (adapter *EinoReasoner) Reason(ctx context.Context, request reasoner.Request) reasoner.Stream {
	request = reasoner.CloneRequest(request)
	if err := reasoner.ValidatePhase5Request(request); err != nil {
		return reasoner.NewErrorStream(request, reasoner.StreamError{
			Class: reasoner.ErrorInvalidRequest, Message: "invalid Eino Reasoner request", Retryable: false,
		})
	}
	messages, err := toEinoMessages(reasoner.MessagesForModel(request))
	if err != nil {
		return reasoner.NewErrorStream(request, reasoner.StreamError{
			Class: reasoner.ErrorInvalidRequest, Message: "invalid internal message", Retryable: false,
		})
	}
	toolInfos, err := toEinoTools(request.Tools)
	if err != nil {
		return reasoner.NewErrorStream(request, reasoner.StreamError{
			Class: reasoner.ErrorInvalidRequest, Message: "invalid Tool Contract conversion", Retryable: false,
		})
	}
	reader, err := adapter.model.Stream(
		ctx,
		messages,
		model.WithTools(toolInfos),
		model.WithMaxTokens(request.Budget.TokenLimit),
	)
	if err != nil {
		return reasoner.NewErrorStream(request, reasoner.ClassifyProviderError(err))
	}
	if reader == nil {
		return reasoner.NewErrorStream(request, reasoner.StreamError{
			Class: reasoner.ErrorInternal, Message: "Eino provider returned no stream", Retryable: false,
		})
	}
	return reasoner.NewNormalizedStream(request, newEinoStream(reader))
}

func toEinoMessages(messages []reasoner.Message) ([]*schema.Message, error) {
	converted := make([]*schema.Message, 0, len(messages))
	for _, message := range messages {
		var role schema.RoleType
		switch message.Role {
		case reasoner.RoleSystem:
			role = schema.System
		case reasoner.RoleUser:
			role = schema.User
		case reasoner.RoleAssistant:
			role = schema.Assistant
		case reasoner.RoleTool:
			role = schema.Tool
		default:
			return nil, fmt.Errorf("unsupported internal role %q", message.Role)
		}
		convertedMessage := &schema.Message{
			Role: role, Content: message.Content,
			ToolCallID: message.ToolCallID, ToolName: message.ToolName,
		}
		for _, call := range message.ToolCalls {
			convertedMessage.ToolCalls = append(convertedMessage.ToolCalls, schema.ToolCall{
				ID: call.ID, Type: "function",
				Function: schema.FunctionCall{Name: call.Name, Arguments: call.Arguments},
			})
		}
		converted = append(converted, convertedMessage)
	}
	return converted, nil
}

func toEinoTools(contracts []tools.Contract) ([]*schema.ToolInfo, error) {
	infos := make([]*schema.ToolInfo, 0, len(contracts))
	for _, contract := range contracts {
		if err := contract.Validate(); err != nil {
			return nil, err
		}
		fullSchema := &jsonschema.Schema{}
		if err := json.Unmarshal(contract.InputSchema, fullSchema); err != nil {
			return nil, fmt.Errorf("Tool %s input schema: %w", contract.Name, err)
		}
		infos = append(infos, &schema.ToolInfo{
			Name: contract.Name,
			Desc: fmt.Sprintf(
				"AgentDock Tool Contract %s; capability=%s; network=%t; idempotency=%s",
				contract.Version,
				contract.Capability,
				contract.Network,
				contract.Idempotency,
			),
			ParamsOneOf: schema.NewParamsOneOfByJSONSchema(fullSchema),
		})
	}
	sort.Slice(infos, func(left, right int) bool { return infos[left].Name < infos[right].Name })
	return infos, nil
}

type toolAccumulator struct {
	ID        string
	Name      string
	Arguments string
}

type einoStream struct {
	reader    *schema.StreamReader[*schema.Message]
	queue     []reasoner.Event
	calls     map[int]*toolAccumulator
	callOrder []int
	nextKey   int
	usage     *reasoner.Usage
	closed    bool
}

func newEinoStream(reader *schema.StreamReader[*schema.Message]) *einoStream {
	return &einoStream{reader: reader, calls: make(map[int]*toolAccumulator)}
}

func (stream *einoStream) Recv() (reasoner.Event, error) {
	for {
		if len(stream.queue) > 0 {
			event := stream.queue[0]
			stream.queue = stream.queue[1:]
			return event, nil
		}
		if stream.closed {
			return reasoner.Event{}, io.EOF
		}
		chunk, err := stream.reader.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return reasoner.Event{}, io.EOF
			}
			failure := reasoner.ClassifyProviderStreamError(err)
			return reasoner.Event{}, &failure
		}
		if chunk == nil {
			return reasoner.Event{}, errors.New("Eino stream returned a nil chunk")
		}
		if chunk.Content != "" {
			stream.queue = append(stream.queue, reasoner.Event{Type: reasoner.EventTextDelta, Text: chunk.Content})
		}
		for _, call := range chunk.ToolCalls {
			if err := stream.accumulateToolCall(call); err != nil {
				failure := reasoner.StreamError{Class: reasoner.ErrorInvalidEvent, Message: "conflicting Eino Tool Call atomic fields", Retryable: false}
				return reasoner.Event{}, &failure
			}
		}
		if chunk.ResponseMeta != nil && chunk.ResponseMeta.Usage != nil {
			if err := stream.recordUsage(chunk.ResponseMeta.Usage); err != nil {
				failure := reasoner.StreamError{Class: reasoner.ErrorInvalidEvent, Message: "Eino Usage decreased across chunks", Retryable: false}
				return reasoner.Event{}, &failure
			}
		}
		if chunk.ResponseMeta != nil && chunk.ResponseMeta.FinishReason != "" {
			stream.flushToolCalls()
			if stream.usage != nil {
				usage := *stream.usage
				stream.queue = append(stream.queue, reasoner.Event{Type: reasoner.EventUsage, Usage: &usage})
			}
			stream.queue = append(stream.queue, reasoner.Event{
				Type: reasoner.EventFinish, Finish: &reasoner.Finish{Reason: chunk.ResponseMeta.FinishReason},
			})
		}
	}
}

func usageEvent(usage *schema.TokenUsage) reasoner.Event {
	total := usage.TotalTokens
	if total == 0 {
		total = usage.PromptTokens + usage.CompletionTokens
	}
	return reasoner.Event{Type: reasoner.EventUsage, Usage: &reasoner.Usage{
		InputTokens: usage.PromptTokens, OutputTokens: usage.CompletionTokens, TotalTokens: total,
	}}
}

func (stream *einoStream) Close() {
	if stream.closed {
		return
	}
	stream.closed = true
	stream.reader.Close()
}

func (stream *einoStream) accumulateToolCall(call schema.ToolCall) error {
	key := -1
	if call.Index != nil {
		key = *call.Index
	} else if call.ID != "" {
		for candidate, accumulated := range stream.calls {
			if accumulated.ID == call.ID {
				key = candidate
				break
			}
		}
	} else if len(stream.calls) == 1 {
		for candidate := range stream.calls {
			key = candidate
		}
	}
	if key < 0 {
		stream.nextKey++
		key = 1_000_000 + stream.nextKey
	}
	accumulated, exists := stream.calls[key]
	if !exists {
		accumulated = &toolAccumulator{}
		stream.calls[key] = accumulated
		stream.callOrder = append(stream.callOrder, key)
	}
	if call.ID != "" {
		if accumulated.ID != "" && accumulated.ID != call.ID {
			return errors.New("conflicting Tool Call ID")
		}
		accumulated.ID = call.ID
	}
	if call.Function.Name != "" {
		if accumulated.Name != "" && accumulated.Name != call.Function.Name {
			return errors.New("conflicting Tool Call name")
		}
		accumulated.Name = call.Function.Name
	}
	accumulated.Arguments += call.Function.Arguments
	return nil
}

func (stream *einoStream) recordUsage(provider *schema.TokenUsage) error {
	event := usageEvent(provider)
	current := event.Usage
	if stream.usage != nil && (current.InputTokens < stream.usage.InputTokens ||
		current.OutputTokens < stream.usage.OutputTokens || current.TotalTokens < stream.usage.TotalTokens) {
		return errors.New("Usage counters decreased")
	}
	stream.usage = current
	return nil
}

func (stream *einoStream) flushToolCalls() {
	for _, key := range stream.callOrder {
		call := stream.calls[key]
		stream.queue = append(stream.queue, reasoner.Event{Type: reasoner.EventToolCall, ToolCall: &reasoner.ToolCall{
			ID: call.ID, Name: call.Name, Arguments: call.Arguments,
		}})
	}
	stream.calls = make(map[int]*toolAccumulator)
	stream.callOrder = nil
}
