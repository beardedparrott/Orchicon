-- File-edit ledger: ground-truth, server-computed diffs for every file a
-- session (execution or Ask conversation) edits. One row per touched path
-- per event; diffs are computed from real file-state snapshot pairs, never
-- parsed from tool-output text. git_confirmed is set by the completion-time
-- git reconciliation; corrective rows (tool='reconcile:git') carry the final
-- worktree truth for paths changed outside tracked tool events. Rows are
-- tenant-scoped; owner is polymorphic (owner_kind + owner_id).
CREATE TABLE IF NOT EXISTS file_edit_ledger (
  id              TEXT PRIMARY KEY,
  tenant_id       TEXT NOT NULL,
  owner_kind      TEXT NOT NULL,
  owner_id        TEXT NOT NULL,
  seq             BIGINT NOT NULL,
  path            TEXT NOT NULL,
  kind            TEXT NOT NULL DEFAULT 'modify',
  unified_diff    TEXT NOT NULL DEFAULT '',
  before_size     BIGINT NOT NULL DEFAULT 0,
  after_size      BIGINT NOT NULL DEFAULT 0,
  before_sha256   TEXT NOT NULL DEFAULT '',
  after_sha256    TEXT NOT NULL DEFAULT '',
  tool            TEXT NOT NULL DEFAULT '',
  is_binary       BOOLEAN NOT NULL DEFAULT FALSE,
  truncated       BOOLEAN NOT NULL DEFAULT FALSE,
  git_confirmed   BOOLEAN NOT NULL DEFAULT FALSE,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, owner_kind, owner_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_file_edit_ledger_owner
  ON file_edit_ledger (tenant_id, owner_kind, owner_id, seq);

-- Row-level security: the uniform tenant_isolation policy (docs/09 §8.5).
-- The data-access layer is the primary isolation layer; RLS is the
-- backstop — even a buggy query cannot leak a ledger row across tenants.
ALTER TABLE file_edit_ledger ENABLE ROW LEVEL SECURITY;
ALTER TABLE file_edit_ledger FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON file_edit_ledger;
CREATE POLICY tenant_isolation ON file_edit_ledger
  FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
