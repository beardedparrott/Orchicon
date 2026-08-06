-- Runtime images: mark which rows are canned "stock" images (seeded from the
-- shipped Dockerfile templates on boot) versus tenant-created custom images.
--
-- `source` distinguishes the two so the canned-image seeder can:
--   - find/create the stock rows for the daemon's shipped images, and
--   - back off from rows the user has diverged from (the seeder only rolls
--     forward pristine stock rows; user edits are never clobbered).
--
-- It is purely informational for the CRUD/build flows (every row builds the
-- same way); it exists so the UI can badge stock images and the seeder can
-- target them precisely.
ALTER TABLE runtime_images
  ADD COLUMN IF NOT EXISTS source text NOT NULL DEFAULT 'custom';
