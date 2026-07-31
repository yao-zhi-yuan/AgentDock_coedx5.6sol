package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ArtifactInput describes bytes that must be complete before registration.
type ArtifactInput struct {
	ID        string
	RunID     string
	AttemptID string
	Type      string
	Content   io.Reader
}

// ArtifactRecord describes metadata registered after the bytes are complete,
// synced, closed, renamed, and hashed.
type ArtifactRecord struct {
	ID        string `json:"artifact_id"`
	RunID     string `json:"run_id"`
	AttemptID string `json:"attempt_id,omitempty"`
	Type      string `json:"type"`
	Digest    string `json:"digest"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	CreatedAt string `json:"created_at"`
}

// PostgresArtifactStore writes complete content before registering metadata.
type PostgresArtifactStore struct {
	events *PostgresEventStore
	root   string
}

// NewPostgresArtifactStore creates an Artifact store rooted at an absolute,
// durable directory.
func NewPostgresArtifactStore(
	events *PostgresEventStore,
	root string,
) (*PostgresArtifactStore, error) {
	if events == nil || events.pool == nil {
		return nil, errors.New("PostgreSQL Event Store is required")
	}
	if root == "" {
		return nil, errors.New("artifact root is required")
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve artifact root: %w", err)
	}
	if err := os.MkdirAll(absoluteRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create artifact root: %w", err)
	}
	return &PostgresArtifactStore{events: events, root: absoluteRoot}, nil
}

// Write persists bytes first and inserts the artifacts row only after the
// complete file and SHA-256 digest exist.
func (store *PostgresArtifactStore) Write(
	ctx context.Context,
	input ArtifactInput,
) (ArtifactRecord, error) {
	if err := ctx.Err(); err != nil {
		return ArtifactRecord{}, err
	}
	if !validArtifactIdentifier(input.ID) ||
		input.RunID == "" ||
		input.Type == "" ||
		input.Content == nil {
		return ArtifactRecord{}, errors.New("artifact ID, Run ID, type, and content are required")
	}

	temporary, err := os.CreateTemp(store.root, ".artifact-*")
	if err != nil {
		return ArtifactRecord{}, fmt.Errorf("create temporary artifact: %w", err)
	}
	temporaryPath := temporary.Name()
	keepTemporary := true
	defer func() {
		if keepTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return ArtifactRecord{}, fmt.Errorf("set artifact permissions: %w", err)
	}

	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(temporary, hasher), input.Content)
	if err != nil {
		temporary.Close()
		return ArtifactRecord{}, fmt.Errorf("write complete artifact: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return ArtifactRecord{}, fmt.Errorf("sync artifact: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return ArtifactRecord{}, fmt.Errorf("close artifact: %w", err)
	}

	digestHex := fmt.Sprintf("%x", hasher.Sum(nil))
	digest := "sha256:" + digestHex
	finalPath := filepath.Join(store.root, input.ID+"-"+digestHex)
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		return ArtifactRecord{}, fmt.Errorf("publish artifact: %w", err)
	}
	keepTemporary = false
	if directory, err := os.Open(store.root); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}

	var createdAt time.Time
	err = store.events.pool.QueryRow(ctx, `
		INSERT INTO artifacts (
			artifact_id, run_id, attempt_id, artifact_type,
			digest, path, size, created_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, CURRENT_TIMESTAMP)
		ON CONFLICT (artifact_id) DO NOTHING
		RETURNING created_at`,
		input.ID,
		input.RunID,
		nullableText(input.AttemptID),
		input.Type,
		digest,
		finalPath,
		size,
	).Scan(&createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		existing, loadErr := store.Get(ctx, input.ID)
		if loadErr != nil {
			return ArtifactRecord{}, fmt.Errorf("load idempotent artifact: %w", loadErr)
		}
		if existing.RunID != input.RunID ||
			existing.AttemptID != input.AttemptID ||
			existing.Type != input.Type ||
			existing.Digest != digest ||
			existing.Path != finalPath ||
			existing.Size != size {
			return ArtifactRecord{}, fmt.Errorf(
				"%w: artifact_id=%q conflicts with durable Artifact",
				ErrIdempotencyConflict,
				input.ID,
			)
		}
		return existing, nil
	}
	if err != nil {
		return ArtifactRecord{}, fmt.Errorf("register complete artifact: %w", err)
	}
	return ArtifactRecord{
		ID:        input.ID,
		RunID:     input.RunID,
		AttemptID: input.AttemptID,
		Type:      input.Type,
		Digest:    digest,
		Path:      finalPath,
		Size:      size,
		CreatedAt: createdAt.UTC().Format(time.RFC3339Nano),
	}, nil
}

// Get loads registered Artifact metadata. Callers that need integrity after
// registration must still re-hash the referenced bytes.
func (store *PostgresArtifactStore) Get(ctx context.Context, artifactID string) (ArtifactRecord, error) {
	var record ArtifactRecord
	var attemptID *string
	var createdAt time.Time
	err := store.events.pool.QueryRow(ctx, `
		SELECT artifact_id, run_id, attempt_id, artifact_type,
			digest, path, size, created_at
		FROM artifacts
		WHERE artifact_id = $1`,
		artifactID,
	).Scan(
		&record.ID,
		&record.RunID,
		&attemptID,
		&record.Type,
		&record.Digest,
		&record.Path,
		&record.Size,
		&createdAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArtifactRecord{}, ErrArtifactNotFound
	}
	if err != nil {
		return ArtifactRecord{}, fmt.Errorf("load Artifact: %w", err)
	}
	if attemptID != nil {
		record.AttemptID = *attemptID
	}
	record.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
	return record, nil
}

// Verify reloads registered metadata and re-hashes the durable bytes. Receipt
// recovery must use this method before trusting an Artifact-backed completion.
func (store *PostgresArtifactStore) Verify(ctx context.Context, artifactID string) (ArtifactRecord, error) {
	if err := ctx.Err(); err != nil {
		return ArtifactRecord{}, err
	}
	record, err := store.Get(ctx, artifactID)
	if err != nil {
		return ArtifactRecord{}, err
	}
	relativePath, err := filepath.Rel(store.root, record.Path)
	if err != nil ||
		relativePath == "." ||
		relativePath == ".." ||
		filepath.IsAbs(relativePath) ||
		strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) {
		return ArtifactRecord{}, fmt.Errorf(
			"%w: artifact_id=%q path is outside the configured root",
			ErrArtifactIntegrity,
			artifactID,
		)
	}
	if err := verifyArtifactRecordBytes(record); err != nil {
		return ArtifactRecord{}, err
	}
	return record, nil
}

func verifyArtifactRecordBytes(record ArtifactRecord) error {
	file, err := os.Open(record.Path)
	if err != nil {
		return fmt.Errorf(
			"%w: artifact_id=%q open bytes: %v",
			ErrArtifactIntegrity,
			record.ID,
			err,
		)
	}
	defer file.Close()
	hasher := sha256.New()
	size, err := io.Copy(hasher, file)
	if err != nil {
		return fmt.Errorf(
			"%w: artifact_id=%q hash bytes: %v",
			ErrArtifactIntegrity,
			record.ID,
			err,
		)
	}
	digest := fmt.Sprintf("sha256:%x", hasher.Sum(nil))
	if digest != record.Digest || size != record.Size {
		return fmt.Errorf(
			"%w: artifact_id=%q metadata_digest=%q actual_digest=%q metadata_size=%d actual_size=%d",
			ErrArtifactIntegrity,
			record.ID,
			record.Digest,
			digest,
			record.Size,
			size,
		)
	}
	return nil
}

func validArtifactIdentifier(value string) bool {
	if value == "" || value == "." || value == ".." || strings.Contains(value, "..") {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' ||
			character == '_' ||
			character == '.' {
			continue
		}
		return false
	}
	return true
}
