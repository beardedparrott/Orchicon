package adapter

import (
	"context"
	"fmt"
	"strings"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/jackc/pgx/v5"
)

// TenantDemandSet collects the host-side adapter demand set for a tenant:
// the tenant's Ask-default model ref ∪ the model refs of its dispatchable
// workers' latest published versions. The per-run step worker set is the
// run-start gate's half of the same set (scheduler.runNeedsServe); both
// halves feed the ONE primitive (AdapterDemandSet), so a run gate that
// says "no opencode" and a host demand set that says "warm opencode" can
// never disagree (AC 7).
//
// The Ask MODE does not contribute: a mode selects a tool policy, never
// the adapter a turn runs on (the transport is resolved from the model ref
// alone).
//
// The tenant's DefaultWorkerModel is NOT added separately: it is only a
// form prefill for new workers, and every worker that actually dispatches
// contributes its own published ref above. The empty-string case is the
// only place the conservative rule applies — an unresolvable/missing ref
// counts as opencode demand, exactly as it does in the run gate.
//
// A missing tenant_settings row is not an error: db.GetTenantSettings
// creates defaults (empty ask ref), which simply contributes nothing.
func TenantDemandSet(ctx context.Context, tx pgx.Tx, tenantID string) (DemandSet, error) {
	settings, err := db.GetTenantSettings(ctx, tx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("adapter: tenant demand set (settings): %w", err)
	}
	var refs []string
	if ref := strings.TrimSpace(settings.DefaultAskOrchiconModel); ref != "" {
		refs = append(refs, ref)
	}
	workerRefs, err := db.ListPublishedWorkerModelRefs(ctx, tx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("adapter: tenant demand set (workers): %w", err)
	}
	refs = append(refs, workerRefs...)
	return AdapterDemandSet(refs...), nil
}
