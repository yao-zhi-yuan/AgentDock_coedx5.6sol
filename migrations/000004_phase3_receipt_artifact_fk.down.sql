BEGIN;

ALTER TABLE action_receipts
    DROP CONSTRAINT IF EXISTS action_receipts_artifact_fk;

COMMIT;
