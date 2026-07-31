BEGIN;

ALTER TABLE action_receipts
    ADD CONSTRAINT action_receipts_artifact_fk
    FOREIGN KEY (artifact_id)
    REFERENCES artifacts(artifact_id)
    ON DELETE RESTRICT;

COMMIT;
