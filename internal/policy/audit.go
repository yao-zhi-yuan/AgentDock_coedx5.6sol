package policy

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AuditKind identifies phase-4 policy and sandbox evidence. These records are
// operational audit facts, not phase-6 verifier evidence.
type AuditKind string

const (
	AuditPolicyAllowed        AuditKind = "policy_allowed"
	AuditPolicyDenied         AuditKind = "policy_denied"
	AuditExecutionCompleted   AuditKind = "execution_completed"
	AuditExecutionFailed      AuditKind = "execution_failed"
	AuditExecutionTimedOut    AuditKind = "execution_timed_out"
	AuditExecutionCanceled    AuditKind = "execution_canceled"
	AuditOutputTruncated      AuditKind = "output_truncated"
	AuditSandboxCreated       AuditKind = "sandbox_created"
	AuditSandboxCreateFailed  AuditKind = "sandbox_create_failed"
	AuditSandboxDestroyed     AuditKind = "sandbox_destroyed"
	AuditSandboxDestroyFailed AuditKind = "sandbox_destroy_failed"
)

// AuditEvent intentionally records environment key names only. Values and
// command output are never copied into the audit timeline.
type AuditEvent struct {
	Kind               AuditKind `json:"kind"`
	RunID              string    `json:"run_id,omitempty"`
	AttemptID          string    `json:"attempt_id,omitempty"`
	RequestedRunID     string    `json:"requested_run_id,omitempty"`
	RequestedAttemptID string    `json:"requested_attempt_id,omitempty"`
	ToolName           string    `json:"tool_name,omitempty"`
	ContractVersion    string    `json:"contract_version,omitempty"`
	PolicyVersion      string    `json:"policy_version,omitempty"`
	Capability         string    `json:"capability,omitempty"`
	ReadOnly           bool      `json:"read_only"`
	AllowedPaths       []string  `json:"allowed_paths,omitempty"`
	Network            bool      `json:"network"`
	Environment        []string  `json:"environment,omitempty"`
	TimeoutMillis      int64     `json:"timeout_millis"`
	OutputLimit        int       `json:"output_limit_bytes"`
	ImageID            string    `json:"image_id,omitempty"`
	CPU                string    `json:"cpu_limit,omitempty"`
	Memory             string    `json:"memory_limit,omitempty"`
	PIDs               int       `json:"pid_limit,omitempty"`
	Reason             string    `json:"reason,omitempty"`
	ExitCode           int       `json:"exit_code"`
	OutputBytes        int       `json:"output_bytes"`
	Timestamp          string    `json:"timestamp"`
}

type AuditRecorder interface {
	Record(context.Context, AuditEvent) error
}

func RecordBounded(recorder AuditRecorder, event AuditEvent) error {
	// The deadline bounds cooperative recorders. It cannot preempt a recorder
	// already blocked in a local mutex, write, flush, or fsync; callers therefore
	// treat audit persistence errors as fail-closed and surface them for retry.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return recorder.Record(ctx, event)
}

type MemoryAuditRecorder struct {
	mu     sync.Mutex
	events []AuditEvent
}

func NewMemoryAuditRecorder() *MemoryAuditRecorder {
	return &MemoryAuditRecorder{}
}

func (recorder *MemoryAuditRecorder) Record(ctx context.Context, event AuditEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if event.Timestamp == "" {
		event.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	recorder.mu.Lock()
	recorder.events = append(recorder.events, event)
	recorder.mu.Unlock()
	return nil
}

func (recorder *MemoryAuditRecorder) Events() []AuditEvent {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]AuditEvent(nil), recorder.events...)
}

type FileAuditRecorder struct {
	mu     sync.Mutex
	file   *os.File
	writer *bufio.Writer
	path   string
}

func NewFileAuditRecorder(path string) (*FileAuditRecorder, error) {
	if path == "" {
		return nil, errors.New("audit artifact path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(absolute, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &FileAuditRecorder{file: file, writer: bufio.NewWriter(file), path: absolute}, nil
}

func (recorder *FileAuditRecorder) Record(ctx context.Context, event AuditEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if event.Timestamp == "" {
		event.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if _, err := recorder.writer.Write(append(payload, '\n')); err != nil {
		return err
	}
	if err := recorder.writer.Flush(); err != nil {
		return err
	}
	return recorder.file.Sync()
}

func (recorder *FileAuditRecorder) Close() error {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.file == nil {
		return nil
	}
	flushErr := recorder.writer.Flush()
	closeErr := recorder.file.Close()
	recorder.file = nil
	if flushErr != nil {
		return flushErr
	}
	return closeErr
}

const Phase4AuditArtifactType = "phase-4-audit-jsonl"

type AuditArtifact struct {
	Type   string `json:"type"`
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// Artifact flushes and hashes the bounded phase-4 JSONL audit timeline. It is
// intentionally not a verifier Artifact and cannot authorize Run success.
func (recorder *FileAuditRecorder) Artifact() (AuditArtifact, error) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.file == nil {
		return AuditArtifact{}, errors.New("audit recorder is closed")
	}
	if err := recorder.writer.Flush(); err != nil {
		return AuditArtifact{}, err
	}
	file, err := os.Open(recorder.path)
	if err != nil {
		return AuditArtifact{}, err
	}
	defer file.Close()
	hasher := sha256.New()
	size, err := io.Copy(hasher, file)
	if err != nil {
		return AuditArtifact{}, err
	}
	return AuditArtifact{
		Type:   Phase4AuditArtifactType,
		Path:   recorder.path,
		Digest: fmt.Sprintf("sha256:%x", hasher.Sum(nil)),
		Size:   size,
	}, nil
}

type unavailableAuditRecorder struct{}

func (unavailableAuditRecorder) Record(context.Context, AuditEvent) error {
	return errors.New("audit recorder is required")
}

func recorderOrUnavailable(recorder AuditRecorder) AuditRecorder {
	if recorder == nil {
		return unavailableAuditRecorder{}
	}
	return recorder
}
