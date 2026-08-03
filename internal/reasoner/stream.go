package reasoner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/agentdock/agentdock-verify/internal/tools"
)

type sliceStream struct {
	events []Event
	err    error
	index  int
	closed bool
}

func (stream *sliceStream) Recv() (Event, error) {
	if stream.closed {
		return Event{}, io.EOF
	}
	if stream.index < len(stream.events) {
		event := cloneEvent(stream.events[stream.index])
		stream.index++
		return event, nil
	}
	if stream.err != nil {
		err := stream.err
		stream.err = nil
		return Event{}, err
	}
	return Event{}, io.EOF
}

func (stream *sliceStream) Close() {
	stream.closed = true
}

type normalizedStream struct {
	request   Request
	source    Stream
	terminal  bool
	usageSeen bool
}

func NewNormalizedStream(request Request, source Stream) Stream {
	if source == nil {
		source = &sliceStream{err: errors.New("nil source stream")}
	}
	return &normalizedStream{request: CloneRequest(request), source: source}
}

func NewEventStream(request Request, events ...Event) Stream {
	return NewNormalizedStream(request, &sliceStream{events: cloneEvents(events)})
}

func NewErrorStream(request Request, failure StreamError) Stream {
	return NewEventStream(request, Event{Type: EventError, Error: &failure})
}

func (stream *normalizedStream) Recv() (Event, error) {
	if stream.terminal {
		return Event{}, io.EOF
	}
	event, err := stream.source.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			stream.terminal = true
			stream.source.Close()
			return errorEvent(ErrorStreamingInterrupted, "model stream ended before Finish", true), nil
		}
		stream.terminal = true
		var failure *StreamError
		if errors.As(err, &failure) {
			stream.source.Close()
			return Event{Type: EventError, Error: cloneStreamError(failure)}, nil
		}
		if errors.Is(err, context.Canceled) {
			stream.source.Close()
			return errorEvent(ErrorCanceled, "reasoner request was canceled", false), nil
		}
		stream.source.Close()
		return errorEvent(ErrorStreamingInterrupted, "model stream was interrupted", true), nil
	}

	if failure := stream.validateEvent(event); failure != nil {
		stream.terminal = true
		stream.source.Close()
		return Event{Type: EventError, Error: failure}, nil
	}
	if event.Type == EventUsage {
		stream.usageSeen = true
	}
	if event.Type == EventFinish || event.Type == EventError {
		stream.terminal = true
		stream.source.Close()
	}
	return cloneEvent(event), nil
}

func (stream *normalizedStream) Close() {
	if stream.source != nil {
		stream.source.Close()
	}
	stream.terminal = true
}

func (stream *normalizedStream) validateEvent(event Event) *StreamError {
	if failure := validateEventShape(event); failure != nil {
		return failure
	}
	switch event.Type {
	case EventToolCall:
		contract, ok := stream.contract(event.ToolCall.Name)
		if !ok {
			if len(stream.request.Tools) == 0 && event.ToolCall.Name == Phase1PatchTool {
				return nil
			}
			return &StreamError{Class: ErrorInvalidTool, Message: "Tool name is not registered"}
		}
		if err := contract.ValidateInput([]byte(event.ToolCall.Arguments)); err != nil {
			return &StreamError{Class: ErrorInvalidToolArguments, Message: "Tool arguments do not match the registered JSON Schema"}
		}
	case EventUsage:
		if stream.usageSeen {
			return &StreamError{Class: ErrorInvalidEvent, Message: "Reasoner turn must contain exactly one Usage event"}
		}
		if stream.request.Budget.TokenLimit > 0 && event.Usage.TotalTokens > stream.request.Budget.TokenLimit {
			return &StreamError{Class: ErrorTokenBudgetExceeded, Message: "Reasoner token budget exceeded", Retryable: false}
		}
	case EventFinish:
		if len(stream.request.Tools) > 0 && !stream.usageSeen {
			return &StreamError{Class: ErrorInvalidEvent, Message: "Reasoner turn requires one Usage event before Finish"}
		}
	}
	return nil
}

func validateEventShape(event Event) *StreamError {
	variantCount := 0
	if event.Text != "" {
		variantCount++
	}
	if event.ToolCall != nil {
		variantCount++
	}
	if event.Usage != nil {
		variantCount++
	}
	if event.Finish != nil {
		variantCount++
	}
	if event.Error != nil {
		variantCount++
	}
	if variantCount != 1 {
		return &StreamError{Class: ErrorInvalidEvent, Message: "normalized event must contain exactly one variant"}
	}
	switch event.Type {
	case EventTextDelta:
		if event.Text == "" || event.ToolCall != nil || event.Usage != nil || event.Finish != nil || event.Error != nil {
			return &StreamError{Class: ErrorInvalidEvent, Message: "invalid Text Delta event"}
		}
	case EventToolCall:
		if event.ToolCall == nil || event.ToolCall.ID == "" || event.ToolCall.Name == "" || event.ToolCall.Arguments == "" {
			return &StreamError{Class: ErrorInvalidEvent, Message: "Tool Call requires id, name, and arguments"}
		}
		if !jsonValid(event.ToolCall.Arguments) {
			return &StreamError{Class: ErrorInvalidToolArguments, Message: "Tool arguments do not match the registered JSON Schema"}
		}
	case EventUsage:
		if event.Usage == nil || event.Usage.InputTokens < 0 || event.Usage.OutputTokens < 0 || event.Usage.TotalTokens < 0 {
			return &StreamError{Class: ErrorInvalidEvent, Message: "Usage token counts must be non-negative"}
		}
		if event.Usage.TotalTokens < event.Usage.InputTokens+event.Usage.OutputTokens {
			return &StreamError{Class: ErrorInvalidEvent, Message: "Usage total is smaller than input plus output"}
		}
	case EventFinish:
		if event.Finish == nil || event.Finish.Reason == "" {
			return &StreamError{Class: ErrorInvalidEvent, Message: "Finish reason is required"}
		}
	case EventError:
		if event.Error == nil || event.Error.Class == "" || event.Error.Message == "" {
			return &StreamError{Class: ErrorInvalidEvent, Message: "normalized Error requires class and message"}
		}
	default:
		return &StreamError{Class: ErrorInvalidEvent, Message: fmt.Sprintf("unsupported normalized event type %q", event.Type)}
	}
	return nil
}

func cloneStreamError(failure *StreamError) *StreamError {
	if failure == nil {
		return nil
	}
	cloned := *failure
	return &cloned
}

func (stream *normalizedStream) contract(name string) (tools.Contract, bool) {
	for _, contract := range stream.request.Tools {
		if contract.Name == name {
			return contract, true
		}
	}
	return tools.Contract{}, false
}

func errorEvent(class ErrorClass, message string, retryable bool) Event {
	return Event{Type: EventError, Error: &StreamError{Class: class, Message: message, Retryable: retryable}}
}

func jsonValid(arguments string) bool {
	var value any
	return json.Unmarshal([]byte(arguments), &value) == nil
}

func cloneEvents(events []Event) []Event {
	cloned := make([]Event, len(events))
	for index := range events {
		cloned[index] = cloneEvent(events[index])
	}
	return cloned
}

func cloneEvent(event Event) Event {
	cloned := event
	if event.ToolCall != nil {
		value := *event.ToolCall
		cloned.ToolCall = &value
	}
	if event.Usage != nil {
		value := *event.Usage
		cloned.Usage = &value
	}
	if event.Finish != nil {
		value := *event.Finish
		cloned.Finish = &value
	}
	if event.Error != nil {
		value := *event.Error
		cloned.Error = &value
	}
	return cloned
}
