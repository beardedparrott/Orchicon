-- First-class categories
CREATE TABLE IF NOT EXISTS "categories" (
  "id" TEXT NOT NULL PRIMARY KEY,
  "tenant_id" TEXT NOT NULL REFERENCES tenants(id),
  "target_type" TEXT NOT NULL CHECK (target_type IN ('worker','workflow','conversation')),
  "name" TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 64),
  "description" TEXT NOT NULL DEFAULT '',
  "slug" TEXT NOT NULL,
  "sort_order" INTEGER NOT NULL DEFAULT 0,
  "created_at" TIMESTAMPTZ NOT NULL DEFAULT now(),
  "updated_at" TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE ("tenant_id", "target_type", "slug"),
  UNIQUE ("tenant_id", "target_type", "name")
);
CREATE INDEX IF NOT EXISTS categories_tenant_target_order_idx ON categories (tenant_id, target_type, sort_order, id);
ALTER TABLE categories ENABLE ROW LEVEL SECURITY;
ALTER TABLE categories FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON categories;
CREATE POLICY tenant_isolation ON categories FOR ALL USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE TABLE IF NOT EXISTS "category_assignments" (
  "tenant_id" TEXT NOT NULL,
  "target_type" TEXT NOT NULL CHECK (target_type IN ('worker','workflow','conversation')),
  "entity_id" TEXT NOT NULL,
  "category_id" TEXT NOT NULL REFERENCES categories(id) ON DELETE CASCADE,
  "created_at" TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY ("tenant_id", "target_type", "entity_id")
);
CREATE INDEX IF NOT EXISTS category_assignments_category_idx ON category_assignments (tenant_id, category_id);
CREATE INDEX IF NOT EXISTS category_assignments_entity_idx ON category_assignments (tenant_id, entity_id);
ALTER TABLE category_assignments ENABLE ROW LEVEL SECURITY;
ALTER TABLE category_assignments FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON category_assignments;
CREATE POLICY tenant_isolation ON category_assignments FOR ALL USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
