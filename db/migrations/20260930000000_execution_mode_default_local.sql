-- Local is the default execution mode; containers are opt-in (runtime).
--
-- CHANGES THE COLUMN DEFAULT ONLY. Existing rows keep the mode they were
-- created with: a pre-existing project is NOT flipped to local, and a
-- pre-existing local project is NOT flipped to runtime. Only projects
-- created after this migration land in 'local' when they send no mode.
ALTER TABLE projects ALTER COLUMN execution_mode SET DEFAULT 'local';
