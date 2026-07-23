-- Execution iteration counter for loop re-ask naming.
-- Tracks the loop iteration this execution belongs to (0 = first
-- dispatch, 1+ = loop_decision re-ask / re-entry). Used by the
-- frontend to display "Work Item Name - Worker Name - Loop #".
ALTER TABLE worker_executions ADD COLUMN IF NOT EXISTS iteration INT NOT NULL DEFAULT 0;
