package store

import (
	"context"
	"errors"

	"github.com/agentdock/agentdock-verify/internal/domain"
)

var (
	ErrRunNotFound         = errors.New("run not found")
	ErrInvalidAppend       = errors.New("invalid event append")
	ErrIdempotencyConflict = errors.New("idempotency key conflicts with existing event")
)

// AppendResult distinguishes an idempotent replay from a physical append.
type AppendResult struct {
	Event    domain.Event
	Appended bool
}

// EventStore is the narrow persistence boundary needed by phase 1.
type EventStore interface {
	Load(context.Context, string) ([]domain.Event, error)
	Append(context.Context, domain.Event) (AppendResult, error)
}
