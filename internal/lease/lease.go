package lease

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agentdock/agentdock-verify/internal/domain"
)

var (
	ErrInvalidLease       = errors.New("invalid lease request")
	ErrWorkerNotFound     = errors.New("worker not registered")
	ErrWorkerRegistered   = errors.New("worker incarnation already registered")
	ErrLeaseNotFound      = errors.New("lease not found")
	ErrLeaseHeld          = errors.New("lease held by another worker")
	ErrLeaseExpired       = errors.New("lease expired")
	ErrStaleLease         = errors.New("stale worker or fencing token")
	ErrReceiptNotFound    = errors.New("action receipt not found")
	ErrReceiptConflict    = errors.New("action receipt conflicts with durable receipt")
	ErrReceiptWithoutPlan = errors.New("action receipt has no matching pending ActionPlanned event")
)

// Worker is the durable registration and process heartbeat projection.
type Worker struct {
	ID           string
	RegisteredAt time.Time
	HeartbeatAt  time.Time
}

// Lease grants temporary authority to one Worker for one Run.
type Lease struct {
	RunID        string
	WorkerID     string
	FencingToken uint64
	ExpiresAt    time.Time
	HeartbeatAt  time.Time
}

// AcquireResult distinguishes a new acquisition/takeover from an idempotent
// observation by the current owner.
type AcquireResult struct {
	Lease
	Acquired bool
	TookOver bool
}

// ActionReceipt is the durable evidence used to recover the gap between an
// external action and its ActionCompleted event.
type ActionReceipt struct {
	ID               string
	RunID            string
	ActionID         string
	ActionType       domain.CommandType
	IdempotencyScope domain.IdempotencyScope
	Output           domain.EventData
	OutputDigest     string
	ArtifactID       string
	ArtifactDigest   string
	CostUnits        int64
	WorkerID         string
	FencingToken     uint64
	CreatedAt        time.Time
}

// ReceiptResult reports whether RecordReceipt inserted a durable row.
type ReceiptResult struct {
	Receipt  ActionReceipt
	Recorded bool
}

// Manager is the phase-3 coordination and receipt contract.
type Manager interface {
	Register(context.Context, string) (Worker, error)
	LookupWorker(context.Context, string) (Worker, error)
	Heartbeat(context.Context, string) (Worker, error)
	Acquire(context.Context, string, string, time.Duration) (AcquireResult, error)
	Renew(context.Context, Lease, time.Duration) (Lease, error)
	Current(context.Context, string) (Lease, error)
	Validate(context.Context, Lease) error
	RecordReceipt(context.Context, Lease, ActionReceipt) (ReceiptResult, error)
	LookupReceipt(context.Context, string, string) (ActionReceipt, error)
	ListReceipts(context.Context, string) ([]ActionReceipt, error)
}

// LeaseError includes both the presented and authoritative owner/token when
// PostgreSQL rejects a stale operation.
type LeaseError struct {
	RunID         string
	WorkerID      string
	FencingToken  uint64
	CurrentWorker string
	CurrentToken  uint64
	Cause         error
}

func (err *LeaseError) Error() string {
	return fmt.Sprintf(
		"%v: run_id=%q worker_id=%q token=%d current_worker=%q current_token=%d",
		err.Cause,
		err.RunID,
		err.WorkerID,
		err.FencingToken,
		err.CurrentWorker,
		err.CurrentToken,
	)
}

func (err *LeaseError) Unwrap() error {
	return err.Cause
}
