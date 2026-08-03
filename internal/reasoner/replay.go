package reasoner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
)

const CurrentCassetteVersion = "agentdock-reasoner-cassette/v1"

type Cassette struct {
	Version               string    `json:"version"`
	SystemContractVersion string    `json:"system_contract_version"`
	ScenarioID            string    `json:"scenario_id"`
	RecordingMode         string    `json:"recording_mode"`
	Redacted              bool      `json:"redacted"`
	Turns                 [][]Event `json:"turns"`
}

type ReplayReasoner struct {
	mu       sync.Mutex
	cassette Cassette
	nextTurn int
}

func NewReplayReasoner(cassette Cassette) (*ReplayReasoner, error) {
	if err := validateCassette(cassette); err != nil {
		return nil, err
	}
	return &ReplayReasoner{cassette: cloneCassette(cassette)}, nil
}

func LoadCassette(reader io.Reader) (Cassette, error) {
	if reader == nil {
		return Cassette{}, errors.New("cassette reader is required")
	}
	var payload bytes.Buffer
	if _, err := io.Copy(&payload, io.LimitReader(reader, (4<<20)+1)); err != nil {
		return Cassette{}, err
	}
	if payload.Len() > 4<<20 {
		return Cassette{}, errors.New("cassette exceeds 4 MiB")
	}
	if containsSecretMarker(payload.String()) {
		return Cassette{}, errors.New("cassette contains a credential-shaped marker")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload.Bytes()))
	decoder.DisallowUnknownFields()
	var cassette Cassette
	if err := decoder.Decode(&cassette); err != nil {
		return Cassette{}, fmt.Errorf("decode cassette: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Cassette{}, errors.New("cassette must contain one JSON value")
		}
		return Cassette{}, fmt.Errorf("decode trailing cassette JSON: %w", err)
	}
	if err := validateCassette(cassette); err != nil {
		return Cassette{}, err
	}
	return cassette, nil
}

func (replay *ReplayReasoner) Reason(ctx context.Context, request Request) Stream {
	if err := ctx.Err(); err != nil {
		failure := ClassifyProviderError(err)
		return NewErrorStream(request, failure)
	}
	replay.mu.Lock()
	defer replay.mu.Unlock()
	if replay.nextTurn >= len(replay.cassette.Turns) {
		return NewErrorStream(request, StreamError{
			Class: ErrorInternal, Message: "recorded cassette has no remaining turn", Retryable: false,
		})
	}
	events := cloneEvents(replay.cassette.Turns[replay.nextTurn])
	replay.nextTurn++
	return NewNormalizedStream(request, &sliceStream{events: events})
}

func validateCassette(cassette Cassette) error {
	if cassette.Version != CurrentCassetteVersion {
		return fmt.Errorf("cassette version %q is not %q", cassette.Version, CurrentCassetteVersion)
	}
	if cassette.SystemContractVersion != CodingAgentSystemContractVersion {
		return fmt.Errorf("cassette System Contract version %q is not %q", cassette.SystemContractVersion, CodingAgentSystemContractVersion)
	}
	if cassette.ScenarioID == "" || len(cassette.Turns) == 0 {
		return errors.New("cassette scenario_id and turns are required")
	}
	if cassette.RecordingMode != "recorded" || !cassette.Redacted {
		return errors.New("cassette requires recording_mode=recorded and redacted=true evidence")
	}
	canonical, err := json.Marshal(cassette)
	if err != nil {
		return err
	}
	if containsSecretMarker(string(canonical)) {
		return errors.New("cassette contains a credential-shaped marker after decoding")
	}
	for index, turn := range cassette.Turns {
		if len(turn) == 0 {
			return fmt.Errorf("cassette turn %d is empty", index)
		}
		if err := validateRecordedTurn(turn); err != nil {
			return fmt.Errorf("cassette turn %d: %w", index, err)
		}
	}
	return nil
}

func validateRecordedTurn(turn []Event) error {
	terminal := false
	usageCount := 0
	for index, event := range turn {
		if terminal {
			return fmt.Errorf("event %d appears after a terminal event", index)
		}
		if failure := validateEventShape(event); failure != nil {
			return failure
		}
		switch event.Type {
		case EventUsage:
			usageCount++
			if usageCount > 1 {
				return errors.New("turn contains multiple Usage events")
			}
		case EventFinish:
			if usageCount != 1 {
				return errors.New("successful turn requires exactly one Usage event before Finish")
			}
			terminal = true
		case EventError:
			terminal = true
		}
	}
	if !terminal {
		return errors.New("turn is missing a terminal Finish or Error event")
	}
	return nil
}

func containsSecretMarker(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{
		`"api_key"`, `"apikey"`, `"access_key"`, `"authorization"`,
		`"password"`, `"client_secret"`, `"private_key"`, `"token"`,
		"-----begin private key-----", "-----begin rsa private key-----",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	for _, pattern := range []*regexp.Regexp{
		regexp.MustCompile(`(?i)bearer\s+[a-z0-9._-]{8,}`),
		regexp.MustCompile(`(?i)(?:sk-|ghp_|github_pat_|xox[bp]-)[a-z0-9_-]{8,}`),
		regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	} {
		if pattern.MatchString(value) {
			return true
		}
	}
	return false
}

func cloneCassette(cassette Cassette) Cassette {
	cloned := cassette
	cloned.Turns = make([][]Event, len(cassette.Turns))
	for index := range cassette.Turns {
		cloned.Turns[index] = cloneEvents(cassette.Turns[index])
	}
	return cloned
}
