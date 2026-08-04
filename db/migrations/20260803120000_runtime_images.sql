-- Runtime images: tenant container image specs built by the runtime daemon.
--
-- Stores the structured build spec (apt/toolchains/env) plus an optional
-- raw Dockerfile override, the resolved tag, and the last build outcome.
-- The daemon always writes the `FROM <base>` line itself and forces the
-- root/chown wrapper, so every built image inherits the base toolchain and
-- the `org.orchicon.runtime-base=true` label (the container-create gate).
-- All tables carry tenant_id with RLS (invariant #5, AGENTS.md).

CREATE TABLE IF NOT EXISTS runtime_images (
  id                  text NOT NULL,
  tenant_id           text NOT NULL,
  name                text NOT NULL,
  slug                text NOT NULL,
  description         text NOT NULL DEFAULT '',
  base_image_ref      text NOT NULL DEFAULT '',
  apt_packages        jsonb NOT NULL DEFAULT '[]',
  toolchains          jsonb NOT NULL DEFAULT '[]',
  env                 jsonb NOT NULL DEFAULT '{}',
  dockerfile_override text NOT NULL DEFAULT '',
  tag                 text NOT NULL DEFAULT '',
  status              text NOT NULL DEFAULT 'draft',
  build_log           text NOT NULL DEFAULT '',
  error               text NOT NULL DEFAULT '',
  version             integer NOT NULL DEFAULT 1,
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),

  PRIMARY KEY (tenant_id, id)
);

CREATE INDEX IF NOT EXISTS idx_runtime_images_tenant
  ON runtime_images (tenant_id);

CREATE INDEX IF NOT EXISTS idx_runtime_images_tenant_status
  ON runtime_images (tenant_id, status);

ALTER TABLE runtime_images ENABLE ROW LEVEL SECURITY;
ALTER TABLE runtime_images FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON runtime_images
  FOR ALL
  USING (tenant_id = current_setting('app.tenant_id', true));

-- Work items: runtime_image tag chosen for the workflow run's container
-- (empty = base image). Stamped by the backend at create/update.
ALTER TABLE work_items
  ADD COLUMN IF NOT EXISTS runtime_image text NOT NULL DEFAULT '';

-- Workflow runs: resolved runtime image tag captured at run start.
ALTER TABLE workflow_runs
  ADD COLUMN IF NOT EXISTS runtime_image text NOT NULL DEFAULT '';
