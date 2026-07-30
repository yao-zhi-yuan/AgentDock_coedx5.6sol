BEGIN;

DROP TRIGGER IF EXISTS events_reject_update_delete ON events;
DROP FUNCTION IF EXISTS reject_event_mutation();
DROP TABLE IF EXISTS leases;
DROP TABLE IF EXISTS artifacts;
DROP TABLE IF EXISTS attempts;
DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS runs;

COMMIT;
