package domain

import (
	"crypto/sha256"
	"fmt"
)

// Decide returns at most one command and has no side effects.
func Decide(state State) Command {
	if !state.Exists || state.Run.ObservedState.Terminal() {
		return Command{Type: CommandNoop}
	}

	if state.Run.DesiredState == DesiredCancelled {
		return commandFor(state, CommandCancelRun)
	}

	if state.Run.DesiredState == DesiredPaused || state.Run.ObservedState == StatusPaused {
		return Command{Type: CommandNoop}
	}

	if state.FailureReason != "" {
		return commandFor(state, CommandFailRun)
	}

	switch state.Run.ObservedState {
	case StatusQueued:
		command := commandFor(state, CommandStartAttempt)
		command.AttemptID = fmt.Sprintf("%s:attempt:%d", state.Run.ID, state.Run.CurrentAttempt+1)
		return command
	case StatusProvisioning:
		return commandFor(state, CommandProvisionWorkspace)
	case StatusReasoning:
		command := commandFor(state, CommandRunReasoner)
		if state.PendingActionID != "" {
			command.ActionID = state.PendingActionID
		}
		return command
	case StatusActing:
		return commandFor(state, CommandApplyPatch)
	case StatusVerifying:
		if state.VerificationPassed {
			return commandFor(state, CommandSucceedRun)
		}
		command := commandFor(state, CommandVerify)
		if state.PendingActionID != "" {
			command.ActionID = state.PendingActionID
		}
		return command
	default:
		return Command{Type: CommandNoop}
	}
}

func commandFor(state State, commandType CommandType) Command {
	material := fmt.Sprintf("%s|%s|%d|%s", state.Run.ID, commandType, state.Run.CurrentAttempt, state.AttemptID)
	digest := sha256.Sum256([]byte(material))
	return Command{
		Type:      commandType,
		RunID:     state.Run.ID,
		ActionID:  fmt.Sprintf("%x", digest[:16]),
		AttemptID: state.AttemptID,
	}
}
