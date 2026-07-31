BEGIN;

CREATE TABLE workers (
    worker_id text PRIMARY KEY,
    registered_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    heartbeat_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);

INSERT INTO workers (worker_id, registered_at, heartbeat_at)
SELECT worker_id, MIN(heartbeat_at), MAX(heartbeat_at)
FROM leases
GROUP BY worker_id
ON CONFLICT (worker_id) DO NOTHING;

ALTER TABLE leases
    ADD CONSTRAINT leases_worker_fk
    FOREIGN KEY (worker_id)
    REFERENCES workers(worker_id)
    ON DELETE RESTRICT;

CREATE TABLE action_receipts (
    receipt_id text PRIMARY KEY,
    run_id text NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
    action_id text NOT NULL,
    action_type text NOT NULL,
    idempotency_scope text NOT NULL
        CHECK (idempotency_scope IN ('scoped-idempotent', 'unsafe')),
    output jsonb NOT NULL,
    output_digest text
        CHECK (output_digest IS NULL OR output_digest ~ '^sha256:[0-9a-f]{64}$'),
    artifact_id text,
    cost_units bigint NOT NULL DEFAULT 0 CHECK (cost_units >= 0),
    worker_id text NOT NULL REFERENCES workers(worker_id) ON DELETE RESTRICT,
    fencing_token bigint NOT NULL CHECK (fencing_token > 0),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT action_receipts_run_action_unique UNIQUE (run_id, action_id)
);

CREATE INDEX workers_heartbeat_idx ON workers(heartbeat_at);
CREATE INDEX leases_expiry_idx ON leases(expires_at);
CREATE INDEX action_receipts_run_created_idx ON action_receipts(run_id, created_at);
CREATE UNIQUE INDEX action_receipts_run_artifact_unique
    ON action_receipts(run_id, artifact_id)
    WHERE artifact_id IS NOT NULL;

COMMIT;
