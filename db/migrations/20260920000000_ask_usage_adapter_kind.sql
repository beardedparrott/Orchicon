-- Ask Orchicon usage/telemetry across adapters.
--
-- Adds adapter_kind + session_id to usage_records so Ask sessions
-- (which have no execution/task/project) can attribute usage to a specific
-- Ask conversation regardless of which adapter drove the model call. Worker
-- executions leave these empty ('' ) and keep attributing via
-- execution_id / task_id / project_id, so existing worker rollups are
-- unaffected. adapter_kind tags the OTel parity attribute so telemetry
-- identifies the adapter used. Forward-only; the paired _down.sql exists
-- solely for the dev-rollback convention.
ALTER TABLE usage_records
	ADD COLUMN IF NOT EXISTS adapter_kind TEXT NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS session_id TEXT NOT NULL DEFAULT '';
