-- Down migration for 20260712223028_add_outbox.sql

DROP TABLE IF EXISTS "outbox" CASCADE;
DROP INDEX IF EXISTS "outbox_event_id_idx";
DROP INDEX IF EXISTS "outbox_unpublished_idx";
