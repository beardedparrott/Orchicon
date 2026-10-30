-- One row per STOCK variant, enforced by the database.
--
-- REPORTED: the Runtime Images page showed a variant TWICE on a long-lived instance —
--
--     stock-dev   not downloaded   v1
--     stock-dev   ready            v9 · built v9
--
-- The cleanup in stockRowForVariant (application code, run at boot) collapses duplicates, and there was
-- nothing at the database level preventing one. Identity used to be the row's TAG, so the same variant seeded
-- under two tags became two rows; the collapse only fixes that for a database that is re-seeded, which a
-- long-lived instance may not be.
--
-- WHY THIS IS A CORRECTNESS BUG, not tidiness: two rows for one variant means "fetch this one" can build a
-- row that is not the one bound to a work item, and the two diverge silently from then on.
--
-- PARTIAL, on purpose: source='stock' only. A CUSTOM image is tenant-owned and its slug is the operator's
-- namespace — two tenants, or two deliberate images, may legitimately share a name, and a global unique index
-- would break image creation for a reason the operator cannot see.
--
-- THE DUPLICATES MUST GO FIRST, or this migration fails on the very databases that need it. The delete below
-- keeps, per slug, the row with the most progress (built_version, then version) so the row recording real work
-- survives — the same preference stockRowForVariant uses. It deliberately does NOT touch a row whose
-- dockerfile_override has diverged from the shipped template: that row carries operator edits, and deleting it
-- to satisfy a constraint would destroy work to save a schema. If such a row is the duplicate, this migration
-- leaves it and the index creation will fail LOUDLY, which is the correct outcome: a human must decide.
DELETE FROM runtime_images a
USING runtime_images b
WHERE a.tenant_id = b.tenant_id
  AND a.slug = b.slug
  AND a.source = 'stock'
  AND b.source = 'stock'
  AND (a.built_version, a.version, a.id) < (b.built_version, b.version, b.id);

CREATE UNIQUE INDEX IF NOT EXISTS runtime_images_stock_slug_uniq
  ON runtime_images (tenant_id, slug)
  WHERE source = 'stock';
