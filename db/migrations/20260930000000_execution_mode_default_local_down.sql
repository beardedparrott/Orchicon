-- Restore the previous column default. Rows are untouched: rolling this
-- back does not move any project between local and runtime.
ALTER TABLE projects ALTER COLUMN execution_mode SET DEFAULT 'runtime';
