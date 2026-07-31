BEGIN;

UPDATE action_receipts receipts
SET artifact_digest = artifacts.digest
FROM artifacts
WHERE receipts.artifact_id = artifacts.artifact_id
  AND receipts.artifact_digest IS NULL;

ALTER TABLE action_receipts
    DROP CONSTRAINT action_receipts_artifact_digest_pair,
    ADD CONSTRAINT action_receipts_artifact_digest_pair
    CHECK (
        (artifact_id IS NULL AND artifact_digest IS NULL)
        OR (
            artifact_id IS NOT NULL
            AND artifact_digest IS NOT NULL
            AND artifact_digest ~ '^sha256:[0-9a-f]{64}$'
        )
    );

COMMIT;
