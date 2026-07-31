BEGIN;

DROP TABLE IF EXISTS action_receipts;
ALTER TABLE leases DROP CONSTRAINT IF EXISTS leases_worker_fk;
DROP INDEX IF EXISTS leases_expiry_idx;
DROP TABLE IF EXISTS workers;

COMMIT;
