package reasoner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/agentdock/agentdock-verify/internal/tools"
)

var ErrTurnBudgetExceeded = errors.New("coding agent turn budget exceeded")

type CodingAgent struct {
	reasoner         Reasoner
	tools            *tools.Service
	serviceContracts []tools.Contract
	maxTurns         int
}

type CodingAgentResult struct {
	Text          string
	Messages      []Message
	Usage         Usage
	Turns         int
	ToolCallCount int
}

// CodingAgentRequest keeps runtime Tool scope outside the Reasoner input.
type CodingAgentRequest struct {
	RunID     string
	AttemptID string
	Reasoner  Request
}

func NewCodingAgent(runReasoner Reasoner, service *tools.Service, maxTurns int) (*CodingAgent, error) {
	if runReasoner == nil || service == nil || maxTurns <= 0 {
		return nil, errors.New("Reasoner, phase-4 Tool Service, and positive maxTurns are required")
	}
	contracts, err := service.Contracts()
	if err != nil {
		return nil, fmt.Errorf("phase-4 Tool Service contracts: %w", err)
	}
	return &CodingAgent{
		reasoner: runReasoner, tools: service, serviceContracts: contracts, maxTurns: maxTurns,
	}, nil
}

// Run performs model/tool turns only. It does not write lifecycle events,
// decide verification, or implement phase-6 repair semantics.
func (agent *CodingAgent) Run(ctx context.Context, request CodingAgentRequest) (CodingAgentResult, error) {
	request.Reasoner = CloneRequest(request.Reasoner)
	if request.RunID == "" || request.AttemptID == "" {
		return CodingAgentResult{}, errors.New("Coding Agent run_id and attempt_id are required")
	}
	if err := ValidatePhase5Request(request.Reasoner); err != nil {
		return CodingAgentResult{}, fmt.Errorf("invalid Coding Agent request: %w", err)
	}
	if !tools.ContractSetsEqual(request.Reasoner.Tools, agent.serviceContracts) {
		return CodingAgentResult{}, errors.New("Reasoner Tool Contracts do not match the bound phase-4 Tool Service")
	}
	result := CodingAgentResult{Messages: cloneMessages(request.Reasoner.Messages)}
	remainingTokens := request.Reasoner.Budget.TokenLimit
	for turn := 1; turn <= agent.maxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			return CodingAgentResult{}, err
		}
		turnRequest := request.Reasoner
		turnRequest.Messages = cloneMessages(result.Messages)
		turnRequest.Budget.TokenLimit = remainingTokens
		turnResult, err := Collect(agent.reasoner.Reason(ctx, turnRequest))
		if err != nil {
			return CodingAgentResult{}, err
		}
		result.Turns = turn
		result.Text += turnResult.Output
		result.Usage.InputTokens += turnResult.Usage.InputTokens
		result.Usage.OutputTokens += turnResult.Usage.OutputTokens
		result.Usage.TotalTokens += turnResult.Usage.TotalTokens
		remainingTokens -= turnResult.Usage.TotalTokens
		if remainingTokens < 0 {
			return CodingAgentResult{}, &StreamError{
				Class: ErrorTokenBudgetExceeded, Message: "Coding Agent cumulative token budget exceeded", Retryable: false,
			}
		}

		assistant := Message{Role: RoleAssistant, Content: turnResult.Output, ToolCalls: append([]ToolCall(nil), turnResult.ToolCalls...)}
		result.Messages = append(result.Messages, assistant)
		if len(turnResult.ToolCalls) == 0 {
			if turnResult.FinishReason == "tool_calls" {
				return CodingAgentResult{}, fmt.Errorf("%w: Finish requested Tool calls but none were normalized", ErrInvalidResult)
			}
			return result, nil
		}
		for _, call := range turnResult.ToolCalls {
			response, invokeErr := agent.tools.Invoke(ctx, tools.Invocation{
				RunID: request.RunID, AttemptID: request.AttemptID,
				ToolName: call.Name, Input: json.RawMessage(call.Arguments),
			})
			if invokeErr != nil {
				return CodingAgentResult{}, fmt.Errorf("invoke Tool %s through Tool Contract service: %w", call.Name, invokeErr)
			}
			payload, err := json.Marshal(response)
			if err != nil {
				return CodingAgentResult{}, err
			}
			result.Messages = append(result.Messages, Message{
				Role: RoleTool, Content: string(payload), ToolCallID: call.ID, ToolName: call.Name,
			})
			result.ToolCallCount++
		}
	}
	return CodingAgentResult{}, ErrTurnBudgetExceeded
}

func cloneMessages(messages []Message) []Message {
	cloned := make([]Message, len(messages))
	for index := range messages {
		cloned[index] = cloneMessage(messages[index])
	}
	return cloned
}
