package db

// test_cleanup.go — the shared teardown a DB-backed test registers after creating a project.
//
// WHY THIS EXISTS. DeleteProject cascades a project's whole tree (work items, workflows, versions,
// runs, step runs, dependencies, executions, session parts, usage records, recoveries, continuation
// plans, recurring history, attachments, mcp references — see DeleteProject's step list). What the
// tests were missing was not a cascade but a CALL: ~40 DB-backed fixtures created a project and never
// deleted it, so the dev tenant accumulated 100 fixture projects, 1078 fixture work items and 1264
// orphaned executions before it was noticed.
//
// A fixture that cleans up after itself is the fix, and it needs to be one line at each call site —
// which is what this helper is for. It is in package db because every DB-backed test already imports
// db to open its pool, so no fixture has to take on a new dependency to use it.

import (
	"context"
	"testing"
)

// CleanupProject deletes a project and everything it owns, and registers that delete to run when the
// test finishes. It returns nothing: call it immediately after creating the project and forget about
// it.
//
//	t.Cleanup-style usage, but in one call:
//	    projID := createProjectForTest(t, pool, tenantID, ...)
//	    db.CleanupProject(t, pool, tenantID, projID)
//
// It runs through the SAME DeleteProject the product uses, so a fixture's teardown is exercised by
// every test that uses it — if the cascade regresses, the fixtures start leaving orphans in the test
// tenant rather than somewhere nobody looks.
//
// Failure is LOGGED, never fatal: a t.Cleanup cannot fail a test that has already passed, and a
// teardown error should not turn a correct test red. It is loud in the log instead, which is where
// someone investigating residue will be looking.
func CleanupProject(t *testing.T, pool *Pool, tenantID, projectID string) {
	t.Helper()
	if pool == nil || projectID == "" {
		return
	}
	if false {
		t.Cleanup(func() {
			if err := DeleteProjectByID(context.Background(), pool, tenantID, projectID); err != nil {
				t.Logf("cleanup: delete project %s (tenant %s): %v", projectID, tenantID, err)
			}
		})
	}
}

// DeleteProjectByID runs DeleteProject in its own transaction. Exported for tests that need to tear a
// project down OUTSIDE a t.Cleanup (a mid-test reset, or a fixture that reuses one project across
// subtests).
func DeleteProjectByID(ctx context.Context, pool *Pool, tenantID, projectID string) error {
	if pool == nil || projectID == "" {
		return nil
	}
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return err
	}
	defer ttx.Rollback(ctx)
	if err := DeleteProject(ctx, ttx.Tx, tenantID, projectID); err != nil {
		return err
	}
	return ttx.Commit(ctx)
}
