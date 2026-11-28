package askorchicon

import "github.com/beardedparrott/orchicon/internal/db"

func dbUsageRowForTest() db.UsageRecordRow {
	return db.UsageRecordRow{
		ID: "u1", TenantID: "tnt", ProjectID: "p1", TaskID: "wi1",
		ExecutionID: "exec1", WorkflowRunID: "run1", SessionID: "",
		TotalTokens: 1234, CostUSD: 0.5,
	}
}
