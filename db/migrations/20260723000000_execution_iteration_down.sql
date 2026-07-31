-- Down migration for 20260723000000_execution_iteration.sql

ALTER TABLE worker_executions DROP COLUMN IF EXISTS iteration;
