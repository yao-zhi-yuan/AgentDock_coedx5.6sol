BEGIN;

ALTER TABLE attempts
    ADD CONSTRAINT attempts_run_attempt_unique UNIQUE (run_id, attempt_id);

ALTER TABLE artifacts
    ADD CONSTRAINT artifacts_run_attempt_fk
    FOREIGN KEY (run_id, attempt_id)
    REFERENCES attempts(run_id, attempt_id)
    ON DELETE RESTRICT;

COMMIT;
