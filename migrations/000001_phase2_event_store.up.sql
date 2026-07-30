BEGIN;

CREATE TABLE runs (
    run_id text PRIMARY KEY,
    scenario_id text NOT NULL,
    spec_hash text NOT NULL,
    desired_state text NOT NULL,
    observed_state text NOT NULL,
    current_attempt integer NOT NULL DEFAULT 0 CHECK (current_attempt >= 0),
    version bigint NOT NULL DEFAULT 0 CHECK (version >= 0),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    checkpoint_seq bigint,
    checkpoint_state jsonb,
    checkpoint_state_digest text,
    checkpoint_event_digest text,
    checkpoint_created_at timestamptz,
    CONSTRAINT runs_checkpoint_complete CHECK (
        (checkpoint_seq IS NULL
            AND checkpoint_state IS NULL
            AND checkpoint_state_digest IS NULL
            AND checkpoint_event_digest IS NULL
            AND checkpoint_created_at IS NULL)
        OR
        (checkpoint_seq IS NOT NULL
            AND checkpoint_seq > 0
            AND checkpoint_state IS NOT NULL
            AND checkpoint_state_digest IS NOT NULL
            AND checkpoint_event_digest IS NOT NULL
            AND checkpoint_created_at IS NOT NULL)
    )
);

CREATE TABLE events (
    run_id text NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
    seq bigint NOT NULL CHECK (seq > 0),
    event_type text NOT NULL,
    payload_version integer NOT NULL CHECK (payload_version > 0),
    payload jsonb NOT NULL,
    idempotency_key text NOT NULL,
    causation_id text,
    correlation_id text,
    worker_id text,
    fencing_token bigint CHECK (fencing_token IS NULL OR fencing_token >= 0),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT events_run_seq_unique UNIQUE (run_id, seq),
    CONSTRAINT events_run_idempotency_unique UNIQUE (run_id, idempotency_key)
);

CREATE INDEX events_run_created_at_idx ON events(run_id, created_at);

CREATE TABLE attempts (
    attempt_id text PRIMARY KEY,
    run_id text NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
    number integer NOT NULL CHECK (number > 0),
    workspace_digest text,
    reason text,
    started_at timestamptz NOT NULL,
    finished_at timestamptz,
    CONSTRAINT attempts_run_number_unique UNIQUE (run_id, number)
);

CREATE TABLE artifacts (
    artifact_id text PRIMARY KEY,
    run_id text NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
    attempt_id text REFERENCES attempts(attempt_id) ON DELETE RESTRICT,
    artifact_type text NOT NULL,
    digest text NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
    path text NOT NULL,
    size bigint NOT NULL CHECK (size >= 0),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX artifacts_run_digest_idx ON artifacts(run_id, digest);

CREATE TABLE leases (
    run_id text PRIMARY KEY REFERENCES runs(run_id) ON DELETE CASCADE,
    worker_id text NOT NULL,
    fencing_token bigint NOT NULL CHECK (fencing_token > 0),
    expires_at timestamptz NOT NULL,
    heartbeat_at timestamptz NOT NULL
);

CREATE FUNCTION reject_event_mutation() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'events are append-only';
END;
$$;

CREATE TRIGGER events_reject_update_delete
BEFORE UPDATE OR DELETE ON events
FOR EACH ROW EXECUTE FUNCTION reject_event_mutation();

COMMIT;
