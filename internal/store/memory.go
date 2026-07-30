package store

import (
	"context"
	"fmt"
	"sync"

	"github.com/agentdock/agentdock-verify/internal/domain"
)

// MemoryEventStore is the single-process phase-1 store. It validates the full
// candidate log before making an append visible.
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

// Append assigns the next sequence number and deduplicates by idempotency key.
func (store *MemoryEventStore) Append(ctx context.Context, event domain.Event) (AppendResult, error) {
	if err := ctx.Err(); err != nil {
		return AppendResult{}, err
	}
	if event.RunID == "" || event.IdempotencyKey == "" || event.Seq != 0 {
		return AppendResult{}, fmt.Errorf("%w: run_id and idempotency_key are required and seq must be zero", ErrInvalidAppend)
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
	return existing == replay
}
