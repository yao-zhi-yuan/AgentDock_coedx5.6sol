package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/agentdock/agentdock-verify/internal/domain"
)

var (
	ErrRunNotFound           = errors.New("run not found")
	ErrInvalidAppend         = errors.New("invalid event append")
	ErrVersionConflict       = errors.New("run version conflict")
	ErrIdempotencyConflict   = errors.New("idempotency key conflicts with existing event")
	ErrSensitivePayload      = errors.New("event payload contains credential material")
	ErrStaleFencingToken     = errors.New("stale worker fencing token")
	ErrMissingActionReceipt  = errors.New("action completion has no matching durable receipt")
	ErrActionReceiptMismatch = errors.New("action completion does not match durable receipt")
	ErrArtifactNotFound      = errors.New("artifact not found")
	ErrArtifactIntegrity     = errors.New("artifact integrity check failed")
)

// FencingTokenError reports the presented and authoritative Worker authority.
type FencingTokenError struct {
	RunID         string
	WorkerID      string
	FencingToken  uint64
	CurrentWorker string
	CurrentToken  uint64
	Expired       bool
}

func (err *FencingTokenError) Error() string {
	return fmt.Sprintf(
		"%s: run_id=%q worker_id=%q token=%d current_worker=%q current_token=%d expired=%t",
		ErrStaleFencingToken,
		err.RunID,
		err.WorkerID,
		err.FencingToken,
		err.CurrentWorker,
		err.CurrentToken,
		err.Expired,
	)
}

func (err *FencingTokenError) Unwrap() error {
	return ErrStaleFencingToken
}

// VersionConflictError reports the expected and authoritative Run versions.
type VersionConflictError struct {
	RunID    string
	Expected uint64
	Actual   uint64
}

func (err *VersionConflictError) Error() string {
	return fmt.Sprintf(
		"%s: run_id=%q expected=%d actual=%d",
		ErrVersionConflict,
		err.RunID,
		err.Expected,
		err.Actual,
	)
}

func (err *VersionConflictError) Unwrap() error {
	return ErrVersionConflict
}

// AppendResult distinguishes an idempotent replay from a physical append.
type AppendResult struct {
	Event    domain.Event
	Appended bool
}

// EventStore is shared by the in-memory and PostgreSQL implementations.
type EventStore interface {
	Load(context.Context, string) ([]domain.Event, error)
	Rebuild(context.Context, string) (domain.State, error)
	Append(context.Context, uint64, domain.Event) (AppendResult, error)
}

var _ EventStore = (*MemoryEventStore)(nil)
var _ EventStore = (*PostgresEventStore)(nil)
