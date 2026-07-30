BEGIN;

ALTER TABLE artifacts
    DROP CONSTRAINT artifacts_run_attempt_fk;

ALTER TABLE attempts
    DROP CONSTRAINT attempts_run_attempt_unique;

COMMIT;
