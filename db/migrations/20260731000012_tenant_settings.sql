-- Tenant-level settings table. Stores configuration defaults that services
-- read at dispatch time (stall params, default model refs, etc.). Each tenant
-- has exactly one row (created on first read if missing).

CREATE TABLE IF NOT EXISTS tenant_settings (
  tenant_id             text NOT NULL PRIMARY KEY,
  default_worker_model  text NOT NULL DEFAULT '',
  default_ask_orchicon_model text NOT NULL DEFAULT '',
  stall_no_progress_window_seconds    bigint NOT NULL DEFAULT 0,
  stall_no_file_diff_window_seconds   bigint NOT NULL DEFAULT 0,
  stall_text_loop_window_seconds      bigint NOT NULL DEFAULT 0,
  stall_repetition_count              int NOT NULL DEFAULT 0,
  stall_repetition_window_seconds     bigint NOT NULL DEFAULT 0,
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE tenant_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_settings FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON tenant_settings
  FOR ALL
  USING (tenant_id = current_setting('app.tenant_id', true));
