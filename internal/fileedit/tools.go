package fileedit

// Tool identifiers recorded on ledger rows (the `tool` column).
const (
	ToolBatchWrite = "batch_write"
	ToolWrite      = "write"
	ToolEdit       = "edit"
	// ToolOpenCodeWrite / ToolOpenCodeEdit tag entries produced by the
	// opencode built-in write/edit tools (plane-side after-read observer).
	ToolOpenCodeWrite = "opencode:write"
	ToolOpenCodeEdit  = "opencode:edit"
	// ToolReconcileGit tags corrective rows appended by the completion-time
	// git reconciliation (bash edits, missed events, pre-ledger sessions).
	ToolReconcileGit = "reconcile:git"
)
