-- Default "Start immediately on save" (auto_start_workflow) to OFF for new
-- work items (docs/11 §5.1). The original default TRUE made every new item
-- auto-start its bound workflow on save and pre-checked the edit form's
-- "Start immediately on save" checkbox — so merely changing an item's type
-- (kind switch) kicked off a run the user never asked for. The desired
-- behavior is opt-in: false unless the user explicitly checks the box.
ALTER TABLE work_items
  ALTER COLUMN auto_start_workflow SET DEFAULT FALSE;

COMMENT ON COLUMN work_items.auto_start_workflow
  IS 'If true, automatically start the bound workflow on save. Defaults to false (opt-in) so saving edits never starts a run by surprise (docs/11 §2.1).';
