BEGIN;

ALTER TABLE action_receipts
    ADD COLUMN artifact_digest text;

UPDATE action_receipts receipts
SET artifact_digest = artifacts.digest
FROM artifacts
WHERE receipts.artifact_id = artifacts.artifact_id;

ALTER TABLE action_receipts
    ADD CONSTRAINT action_receipts_artifact_digest_pair
    CHECK (
        (artifact_id IS NULL AND artifact_digest IS NULL)
        OR (
            artifact_id IS NOT NULL
            AND artifact_digest ~ '^sha256:[0-9a-f]{64}$'
        )
    );

COMMIT;
