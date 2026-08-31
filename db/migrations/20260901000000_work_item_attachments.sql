-- work_item_attachments: images/screenshots attached to work items (ADR-3)
CREATE TABLE work_item_attachments (
  id            TEXT NOT NULL,
  tenant_id     TEXT NOT NULL,
  work_item_id  TEXT NOT NULL REFERENCES work_items(id) ON DELETE CASCADE,
  project_id    TEXT NOT NULL,
  name          TEXT NOT NULL,
  mime_type     TEXT NOT NULL,
  size_bytes    BIGINT NOT NULL,
  blob_ref      TEXT NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_by    TEXT
);
CREATE INDEX idx_wi_attachments_work_item ON work_item_attachments(tenant_id, work_item_id, created_at);
ALTER TABLE work_item_attachments ENABLE ROW LEVEL SECURITY;
ALTER TABLE work_item_attachments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON work_item_attachments FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));
