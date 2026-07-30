package domain_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/agentdock/agentdock-verify/internal/domain"
)

func TestReduceGoldenEventsMatchesGoldenStateFieldByField(t *testing.T) {
	eventsPath := filepath.Join("..", "..", "testdata", "golden", "phase-2-events.json")
	statePath := filepath.Join("..", "..", "testdata", "golden", "phase-2-state.json")

	var events []domain.Event
	readJSONFile(t, eventsPath, &events)
	var want domain.State
	readJSONFile(t, statePath, &want)

	got, err := domain.Reduce(events)
	if err != nil {
		t.Fatalf("Reduce(golden events) error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Fatalf("Reduce(golden events) mismatch\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
}

func TestReduceFromCheckpointMatchesFullReductionAcross1000Events(t *testing.T) {
	events := thousandValidEvents("run-thousand")
	want, err := domain.Reduce(events)
	if err != nil {
		t.Fatalf("Reduce(1000 events) error = %v", err)
	}

	const checkpointSeq = 501
	checkpoint, err := domain.Reduce(events[:checkpointSeq])
	if err != nil {
		t.Fatalf("Reduce(checkpoint prefix) error = %v", err)
	}
	got, err := domain.ReduceFromCheckpoint(checkpoint, events[checkpointSeq:])
	if err != nil {
		t.Fatalf("ReduceFromCheckpoint() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("checkpoint rebuild differs field-by-field\n got: %#v\nwant: %#v", got, want)
	}
}

func readJSONFile(t *testing.T, path string, target any) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	if err := json.Unmarshal(content, target); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v", path, err)
	}
}

func thousandValidEvents(runID string) []domain.Event {
	events := make([]domain.Event, 0, 1000)
	events = append(events, domain.Event{
		RunID:          runID,
		Seq:            1,
		Type:           domain.EventRunCreated,
		PayloadVersion: domain.CurrentEventPayloadVersion,
		Data:           domain.EventData{ScenarioID: "scenario", SpecHash: "spec"},
		IdempotencyKey: "created",
		CorrelationID:  runID,
	})
	for pair := 0; pair < 499; pair++ {
		pauseSeq := uint64(len(events) + 1)
		events = append(events, domain.Event{
			RunID:          runID,
			Seq:            pauseSeq,
			Type:           domain.EventDesiredStateChanged,
			PayloadVersion: domain.CurrentEventPayloadVersion,
			Data:           domain.EventData{DesiredState: domain.DesiredPaused},
			IdempotencyKey: "pause-" + formatInt(pair),
			CorrelationID:  runID,
		})
		events = append(events, domain.Event{
			RunID:          runID,
			Seq:            pauseSeq + 1,
			Type:           domain.EventDesiredStateChanged,
			PayloadVersion: domain.CurrentEventPayloadVersion,
			Data:           domain.EventData{DesiredState: domain.DesiredRunning},
			IdempotencyKey: "resume-" + formatInt(pair),
			CorrelationID:  runID,
		})
	}
	events = append(events, domain.Event{
		RunID:          runID,
		Seq:            1000,
		Type:           domain.EventAttemptStarted,
		PayloadVersion: domain.CurrentEventPayloadVersion,
		Data: domain.EventData{
			AttemptID: runID + ":attempt:1",
			ActionID:  "start-attempt",
			Reason:    "initial",
		},
		IdempotencyKey: "attempt-started",
		CorrelationID:  runID,
	})
	return events
}

func formatInt(value int) string {
	const digits = "0123456789"
	if value == 0 {
		return "0"
	}
	var buffer [20]byte
	index := len(buffer)
	for value > 0 {
		index--
		buffer[index] = digits[value%10]
		value /= 10
	}
	return string(buffer[index:])
}
