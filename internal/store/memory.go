package store

import (
	"context"
	"fmt"
	"sync"

	"github.com/agentdock/agentdock-verify/internal/domain"
)

// MemoryEventStore is the compatible single-process test/demo store. It
// validates the full candidate log before making an append visible.
type MemoryEventStore struct {
	mu              sync.RWMutex
	events          map[string][]domain.Event
	idempotencyKeys map[string]map[string]int
}

// NewMemoryEventStore returns an empty race-safe store.
func NewMemoryEventStore() *MemoryEventStore {
	return &MemoryEventStore{
		events:          make(map[string][]domain.Event),
		idempotencyKeys: make(map[string]map[string]int),
	}
}

// Load returns a defensive copy of the ordered event log.
func (store *MemoryEventStore) Load(ctx context.Context, runID string) ([]domain.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	store.mu.RLock()
	defer store.mu.RUnlock()
	events, ok := store.events[runID]
	if !ok {
		return nil, ErrRunNotFound
	}
	return append([]domain.Event(nil), events...), nil
}

// Rebuild reduces the authoritative in-memory event log without a separate
// process cache.
func (store *MemoryEventStore) Rebuild(ctx context.Context, runID string) (domain.State, error) {
	events, err := store.Load(ctx, runID)
	if err != nil {
		return domain.State{}, err
	}
	return domain.Reduce(events)
}

// Append assigns the next sequence number and deduplicates by idempotency key.
func (store *MemoryEventStore) Append(
	ctx context.Context,
	expectedVersion uint64,
	event domain.Event,
) (AppendResult, error) {
	if err := ctx.Err(); err != nil {
		return AppendResult{}, err
	}
	if err := validateAppendInput(event); err != nil {
		return AppendResult{}, err
	}
	if event.PayloadVersion == 0 {
		event.PayloadVersion = domain.CurrentEventPayloadVersion
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if index, duplicate := store.idempotencyKeys[event.RunID][event.IdempotencyKey]; duplicate {
		existing := store.events[event.RunID][index]
		if !sameIdempotentEvent(existing, event) {
			return AppendResult{}, fmt.Errorf(
				"%w: run_id=%q idempotency_key=%q",
				ErrIdempotencyConflict,
				event.RunID,
				event.IdempotencyKey,
			)
		}
		return AppendResult{Event: existing, Appended: false}, nil
	}

	current := store.events[event.RunID]
	if uint64(len(current)) != expectedVersion {
		return AppendResult{}, &VersionConflictError{
			RunID:    event.RunID,
			Expected: expectedVersion,
			Actual:   uint64(len(current)),
		}
	}
	event.Seq = uint64(len(current) + 1)
	candidate := append(append([]domain.Event(nil), current...), event)
	if _, err := domain.Reduce(candidate); err != nil {
		return AppendResult{}, err
	}

	store.events[event.RunID] = candidate
	if store.idempotencyKeys[event.RunID] == nil {
		store.idempotencyKeys[event.RunID] = make(map[string]int)
	}
	store.idempotencyKeys[event.RunID][event.IdempotencyKey] = len(candidate) - 1
	return AppendResult{Event: event, Appended: true}, nil
}

func sameIdempotentEvent(existing, replay domain.Event) bool {
	existing.Seq = 0
	replay.Seq = 0
	existing.CreatedAt = ""
	replay.CreatedAt = ""
	if replay.PayloadVersion == 0 {
		replay.PayloadVersion = domain.CurrentEventPayloadVersion
	}
	return existing == replay
}
