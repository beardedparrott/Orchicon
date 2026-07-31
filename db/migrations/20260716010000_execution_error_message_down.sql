-- Down migration for 20260716010000_execution_error_message.sql

ALTER TABLE "worker_executions" DROP COLUMN IF EXISTS "error_message";
