BEGIN;

ALTER TABLE action_receipts
    DROP CONSTRAINT IF EXISTS action_receipts_artifact_digest_pair,
    DROP COLUMN IF EXISTS artifact_digest;

COMMIT;
