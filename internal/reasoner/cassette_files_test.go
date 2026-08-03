package reasoner_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentdock/agentdock-verify/internal/reasoner"
)

func TestCommittedRecordedCassettesAreValidAndCredentialFree(t *testing.T) {
	for _, name := range []string{"phase5-normalize-name.json", "phase5-divide-zero.json"} {
		t.Run(name, func(t *testing.T) {
			file, err := os.Open(filepath.Join("..", "..", "testdata", "cassettes", name))
			if err != nil {
				t.Fatal(err)
			}
			cassette, loadErr := reasoner.LoadCassette(file)
			closeErr := file.Close()
			if loadErr != nil {
				t.Fatalf("LoadCassette() error = %v", loadErr)
			}
			if closeErr != nil {
				t.Fatal(closeErr)
			}
			if len(cassette.Turns) != 4 {
				t.Fatalf("turns = %d, want 4", len(cassette.Turns))
			}
		})
	}
}

func TestCassetteLoaderRejectsCredentialMarkers(t *testing.T) {
	file, err := os.Open(filepath.Join("..", "..", "testdata", "cassettes", "phase5-normalize-name.json"))
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(file.Name())
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	content = append(content[:len(content)-2], []byte(",\"api_key\":\"secret\"}\n")...)
	if _, err := reasoner.LoadCassette(bytes.NewReader(content)); err == nil {
		t.Fatal("LoadCassette() accepted credential-shaped content")
	}
}

func TestCassetteValidationRejectsMalformedTerminalState(t *testing.T) {
	base := reasoner.Cassette{
		Version: reasoner.CurrentCassetteVersion, SystemContractVersion: reasoner.CodingAgentSystemContractVersion,
		ScenarioID: "malformed", RecordingMode: "recorded", Redacted: true,
	}
	for name, turn := range map[string][]reasoner.Event{
		"missing usage": {
			{Type: reasoner.EventFinish, Finish: &reasoner.Finish{Reason: "stop"}},
		},
		"duplicate usage": {
			{Type: reasoner.EventUsage, Usage: &reasoner.Usage{TotalTokens: 1}},
			{Type: reasoner.EventUsage, Usage: &reasoner.Usage{TotalTokens: 1}},
			{Type: reasoner.EventFinish, Finish: &reasoner.Finish{Reason: "stop"}},
		},
		"after finish": {
			{Type: reasoner.EventUsage, Usage: &reasoner.Usage{TotalTokens: 1}},
			{Type: reasoner.EventFinish, Finish: &reasoner.Finish{Reason: "stop"}},
			{Type: reasoner.EventTextDelta, Text: "trailing"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := base
			candidate.Turns = [][]reasoner.Event{turn}
			if _, err := reasoner.NewReplayReasoner(candidate); err == nil {
				t.Fatal("NewReplayReasoner accepted malformed terminal state")
			}
		})
	}
}

func TestCassetteLoaderRejectsEscapedAndCommonCredentialMarkers(t *testing.T) {
	for name, marker := range map[string]string{
		"escaped api key": `gh\u0070_SECRET123`,
		"github token":    `ghp_0123456789abcdef`,
		"slack token":     `xoxb-0123456789`,
		"private key":     `-----BEGIN PRIVATE KEY-----`,
	} {
		t.Run(name, func(t *testing.T) {
			payload := `{"version":"agentdock-reasoner-cassette/v1","system_contract_version":"agentdock-coding-v1","scenario_id":"secret","recording_mode":"recorded","redacted":true,"turns":[[{"type":"text_delta","text":"` + marker + `"},{"type":"usage","usage":{"total_tokens":1}},{"type":"finish","finish":{"reason":"stop"}}]]}`
			if _, err := reasoner.LoadCassette(bytes.NewBufferString(payload)); err == nil {
				t.Fatal("LoadCassette accepted credential-shaped content")
			}
		})
	}
}
