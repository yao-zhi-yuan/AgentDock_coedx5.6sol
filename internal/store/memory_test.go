package store_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/agentdock/agentdock-verify/internal/domain"
	"github.com/agentdock/agentdock-verify/internal/store"
)

func TestMemoryStoreDeduplicatesIdempotencyKey(t *testing.T) {
	ctx := context.Background()
	memory := store.NewMemoryEventStore()
	created := domain.Event{
		RunID:          "run-001",
		Type:           domain.EventRunCreated,
		IdempotencyKey: "created",
	}

	first, err := memory.Append(ctx, 0, created)
	if err != nil {
		t.Fatalf("first Append() error = %v", err)
	}
	second, err := memory.Append(ctx, 0, created)
	if err != nil {
		t.Fatalf("second Append() error = %v", err)
	}
	if !first.Appended || second.Appended {
		t.Fatalf("append flags = first:%t second:%t, want true/false", first.Appended, second.Appended)
	}
	if first.Event.Seq != second.Event.Seq {
		t.Fatalf("deduplicated event changed seq: first=%d second=%d", first.Event.Seq, second.Event.Seq)
	}

	events, err := memory.Load(ctx, "run-001")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("event count = %d, want 1", len(events))
	}
}

func TestMemoryStoreRejectsStaleExpectedVersion(t *testing.T) {
	ctx := context.Background()
	memory := store.NewMemoryEventStore()
	if _, err := memory.Append(ctx, 0, domain.Event{
		RunID:          "run-cas",
		Type:           domain.EventRunCreated,
		IdempotencyKey: "created",
		Data:           domain.EventData{ScenarioID: "scenario", SpecHash: "spec"},
	}); err != nil {
		t.Fatalf("create Append() error = %v", err)
	}

	_, err := memory.Append(ctx, 0, domain.Event{
		RunID:          "run-cas",
		Type:           domain.EventDesiredStateChanged,
		IdempotencyKey: "stale-pause",
		Data:           domain.EventData{DesiredState: domain.DesiredPaused},
	})
	if !errors.Is(err, store.ErrVersionConflict) {
		t.Fatalf("stale Append() error = %v, want version conflict", err)
	}
	events, loadErr := memory.Load(ctx, "run-cas")
	if loadErr != nil {
		t.Fatalf("Load() error = %v", loadErr)
	}
	if len(events) != 1 {
		t.Fatalf("stale append changed event count to %d", len(events))
	}
}

func TestMemoryStoreRejectsCredentialBearingPayload(t *testing.T) {
	ctx := context.Background()
	cases := []string{
		`{"api_key":"do-not-persist"}`,
		`{"env":{"PATH":"/bin","SERVICE_TOKEN":"do-not-persist"}}`,
		`{"database":"postgres://user:password@example.invalid/database"}`,
		"authorization: bearer do-not-persist",
		"sk-proj-0123456789abcdefghijklmnop",
		"postgres://user:password@example.invalid/database",
		"AWS_SECRET_ACCESS_KEY=do-not-persist",
		"SERVICE_TOKEN=do-not-persist",
	}
	for index, payload := range cases {
		memory := store.NewMemoryEventStore()
		_, err := memory.Append(ctx, 0, domain.Event{
			RunID:          fmt.Sprintf("run-secret-%d", index),
			Type:           domain.EventRunCreated,
			IdempotencyKey: "created",
			Data: domain.EventData{
				ScenarioID: "scenario",
				SpecHash:   "spec",
				Output:     payload,
			},
		})
		if !errors.Is(err, store.ErrSensitivePayload) {
			t.Fatalf("credential payload %q Append() error = %v, want sensitive payload rejection", payload, err)
		}
	}
}

func TestMemoryStoreRejectsNestedCredentialJSONInEveryEventDataStringField(t *testing.T) {
	ctx := context.Background()
	dataType := reflect.TypeOf(domain.EventData{})
	for index := 0; index < dataType.NumField(); index++ {
		field := dataType.Field(index)
		if field.Type.Kind() != reflect.String {
			continue
		}
		t.Run(field.Name, func(t *testing.T) {
			data := domain.EventData{ScenarioID: "scenario", SpecHash: "spec"}
			reflect.ValueOf(&data).Elem().Field(index).SetString(
				`{"metadata":[{"OpenAI.Api-Key":"opaque-credential"}]}`,
			)
			memory := store.NewMemoryEventStore()
			_, err := memory.Append(ctx, 0, domain.Event{
				RunID:          "run-all-string-fields-" + field.Name,
				Type:           domain.EventRunCreated,
				IdempotencyKey: "created",
				Data:           data,
			})
			if !errors.Is(err, store.ErrSensitivePayload) {
				t.Fatalf(
					"nested credential in EventData.%s Append() error = %v, want sensitive payload rejection",
					field.Name,
					err,
				)
			}

			normalData := domain.EventData{ScenarioID: "scenario", SpecHash: "spec"}
			reflect.ValueOf(&normalData).Elem().Field(index).SetString(
				`{"metadata":[{"message":"ordinary non-credential text"}]}`,
			)
			normalMemory := store.NewMemoryEventStore()
			if _, err := normalMemory.Append(ctx, 0, domain.Event{
				RunID:          "run-normal-string-field-" + field.Name,
				Type:           domain.EventRunCreated,
				IdempotencyKey: "created",
				Data:           normalData,
			}); err != nil {
				t.Fatalf(
					"normal JSON in EventData.%s Append() error = %v, want success",
					field.Name,
					err,
				)
			}
		})
	}
}

func TestMemoryStoreCredentialJSONVariantsAndNormalText(t *testing.T) {
	ctx := context.Background()
	sensitive := []string{
		`{"env":{"OPENAI_API_KEY":"sk-proj-abcdefghijklmnop"}}`,
		`[{"AWS.Secret-Access-Key":"opaque-credential"}]`,
		`{"SeRvIcE-ToKeN":"opaque-credential"}`,
		`{"wrapper":"{\"OpenAI.Api-Key\":\"opaque-credential\"}"}`,
	}
	for index, reason := range sensitive {
		memory := store.NewMemoryEventStore()
		_, err := memory.Append(ctx, 0, domain.Event{
			RunID:          fmt.Sprintf("run-nested-variant-%d", index),
			Type:           domain.EventRunCreated,
			IdempotencyKey: "created",
			Data: domain.EventData{
				ScenarioID: "scenario",
				SpecHash:   "spec",
				Reason:     reason,
			},
		})
		if !errors.Is(err, store.ErrSensitivePayload) {
			t.Fatalf("nested credential variant %q error = %v, want rejection", reason, err)
		}
	}

	normal := []string{
		`{"environment_name":"staging","token_count":42,"api_key_rotation":"planned"}`,
		`[{"message":"explain password rotation without including credentials"}]`,
		"ordinary reason text with no credential material",
	}
	for index, reason := range normal {
		memory := store.NewMemoryEventStore()
		_, err := memory.Append(ctx, 0, domain.Event{
			RunID:          fmt.Sprintf("run-normal-text-%d", index),
			Type:           domain.EventRunCreated,
			IdempotencyKey: "created",
			Data: domain.EventData{
				ScenarioID: "scenario",
				SpecHash:   "spec",
				Reason:     reason,
			},
		})
		if err != nil {
			t.Fatalf("normal text %q Append() error = %v, want success", reason, err)
		}
	}
}

func TestMemoryStoreRejectsIdempotencyKeyWithDifferentEvent(t *testing.T) {
	ctx := context.Background()
	memory := store.NewMemoryEventStore()
	first := domain.Event{
		RunID:          "run-conflict",
		Type:           domain.EventRunCreated,
		IdempotencyKey: "created",
		Data:           domain.EventData{ScenarioID: "scenario-a", SpecHash: "spec-a"},
	}
	if _, err := memory.Append(ctx, 0, first); err != nil {
		t.Fatalf("first Append() error = %v", err)
	}

	conflict := first
	conflict.Data.ScenarioID = "scenario-b"
	_, err := memory.Append(ctx, 0, conflict)
	if !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("conflicting Append() error = %v, want idempotency conflict", err)
	}

	events, loadErr := memory.Load(ctx, "run-conflict")
	if loadErr != nil {
		t.Fatalf("Load() error = %v", loadErr)
	}
	if len(events) != 1 || events[0].Data.ScenarioID != "scenario-a" {
		t.Fatalf("conflict changed persisted event: %#v", events)
	}
}

func TestMemoryStoreRejectsInvalidTransitionWithoutPersistingIt(t *testing.T) {
	ctx := context.Background()
	memory := store.NewMemoryEventStore()
	_, err := memory.Append(ctx, 0, domain.Event{
		RunID:          "run-001",
		Type:           domain.EventAttemptStarted,
		IdempotencyKey: "attempt",
		Data:           domain.EventData{AttemptID: "attempt-001"},
	})
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("Append() error = %v, want invalid transition", err)
	}

	events, loadErr := memory.Load(ctx, "run-001")
	if !errors.Is(loadErr, store.ErrRunNotFound) {
		t.Fatalf("Load() error = %v, want run not found", loadErr)
	}
	if len(events) != 0 {
		t.Fatalf("invalid event was persisted: %#v", events)
	}
}

func TestMemoryStoreConcurrentDuplicateOnlyAppendsOnce(t *testing.T) {
	ctx := context.Background()
	memory := store.NewMemoryEventStore()
	const writers = 32

	var wg sync.WaitGroup
	results := make(chan store.AppendResult, writers)
	errorsCh := make(chan error, writers)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := memory.Append(ctx, 0, domain.Event{
				RunID:          "run-race",
				Type:           domain.EventRunCreated,
				IdempotencyKey: "created",
			})
			results <- result
			errorsCh <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errorsCh)

	appended := 0
	for result := range results {
		if result.Appended {
			appended++
		}
	}
	for err := range errorsCh {
		if err != nil {
			t.Fatalf("concurrent Append() error = %v", err)
		}
	}
	if appended != 1 {
		t.Fatalf("physical append count = %d, want 1", appended)
	}
}

func TestMemoryStoreConcurrentConflictingCreateOnlyOneWins(t *testing.T) {
	ctx := context.Background()
	memory := store.NewMemoryEventStore()
	events := []domain.Event{
		{
			RunID:          "run-conflicting-create",
			Type:           domain.EventRunCreated,
			IdempotencyKey: "created",
			Data:           domain.EventData{ScenarioID: "scenario-a", SpecHash: "spec"},
		},
		{
			RunID:          "run-conflicting-create",
			Type:           domain.EventRunCreated,
			IdempotencyKey: "created",
			Data:           domain.EventData{ScenarioID: "scenario-b", SpecHash: "spec"},
		},
	}

	var wg sync.WaitGroup
	errorsCh := make(chan error, len(events))
	for _, event := range events {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := memory.Append(ctx, 0, event)
			errorsCh <- err
		}()
	}
	wg.Wait()
	close(errorsCh)

	successes := 0
	conflicts := 0
	for err := range errorsCh {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, store.ErrIdempotencyConflict):
			conflicts++
		default:
			t.Fatalf("concurrent Append() error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent creates = successes:%d conflicts:%d, want 1/1", successes, conflicts)
	}

	persisted, err := memory.Load(ctx, "run-conflicting-create")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(persisted) != 1 {
		t.Fatalf("persisted event count = %d, want 1", len(persisted))
	}
}
