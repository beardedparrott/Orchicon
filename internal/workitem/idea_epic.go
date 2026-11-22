package workitem

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/jackc/pgx/v5"
)

// THE IDEA CLOUD EPIC — the one anchor every automation-spawned idea hangs from.
//
// WHY IT EXISTS. Ideas are spawned by an automation worker whose model chooses
// each idea's `kind` and `parent_id` freely. Before this, a fire could create its
// OWN epic and parent its ideas under it (which is exactly what the reported run
// did: one idea-state epic, seven idea-state children). That shape is a trap,
// because the Idea Cloud is a FLAT triage list — an operator promotes ONE child,
// and PromoteIdea flips only that row:
//
//	parent epic  → still status='idea'  → excluded from every normal list query
//	child        → pending             → now a normal work item
//
// and the promoted child is a pending row pointing at a parent no normal view can
// resolve. The tree dropped it entirely (see findOrphanIds in the frontend), the
// detail page printed a bare ULID where the parent's title belongs. Every single
// symptom traced back to this one shape.
//
// THE FIX IS STRUCTURAL, NOT A GUARD. Every spawned idea is parented to a single
// per-project epic, and that epic is NEVER reduced to a per-fire construct:
//
//   - exactly one per project, named IdeaCloudEpicTitle, resolved-or-created;
//   - it is a plain top-level epic, so promoting any number of its children can
//     never strand them — the parent is already visible in every normal view;
//   - ideas spawn as `task` (the flat, unopinionated kind), NOT `feature`: a
//     promotion decision is the operator's, and a task they can freely retype or
//     re-parent afterwards is a smaller commitment than a feature.
//
// The epic itself is intentionally NOT your backlog's epic — it is a container so
// triage has somewhere to live, and an operator who promotes an idea can move it
// under a real epic at that point.
const IdeaCloudEpicTitle = "Idea Cloud"

// IdeaCloudEpicDescription explains the container on the epic itself, so an
// operator who opens it is not left guessing why it exists or whether it is real
// work.
const IdeaCloudEpicDescription = "Automation-spawned ideas awaiting triage. " +
	"Each child is an idea-state work item produced by a recurring research run; " +
	"approve one to promote it into the backlog (where you can retype or re-parent it), " +
	"or dismiss it to record a durable rejection. This epic is a container, not planned work."

// ideaEpicLockKey is the advisory-lock key that serialises resolve-or-create for
// the Idea Cloud epic within a tenant+project. Two concurrent fires spawning their
// first ideas would otherwise both see "no epic" and both create one, leaving two
// Idea Cloud epics and splitting triage across them.
const ideaEpicLockKey = 0x1DEA_C10D

// ResolveIdeaCloudEpic returns the project's Idea Cloud epic, creating it on first
// use. It runs INSIDE the caller's tenant transaction so the create is atomic with
// the idea that needed it: a failed idea spawn never leaves an orphan epic behind.
//
// The advisory lock is transaction-scoped (pg_advisory_xact_lock), so it releases
// on commit/rollback with no cleanup path of its own.
func ResolveIdeaCloudEpic(ctx context.Context, tx pgx.Tx, tenantID, projectID string) (db.WorkItemRow, error) {
	if projectID == "" {
		return db.WorkItemRow{}, errors.New("project_id must not be empty")
	}
	// Serialise resolve-or-create. Two keys: the tenant ns is constant so the lock
	// is namespaced per (tenant, project) rather than global.
	lockID := int64(ideaEpicLockKey)<<20 ^ int64(hashString(tenantID+":"+projectID))
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockID); err != nil {
		return db.WorkItemRow{}, fmt.Errorf("lock idea cloud epic: %w", err)
	}
	existing, err := findIdeaCloudEpic(ctx, tx, tenantID, projectID)
	if err != nil {
		return db.WorkItemRow{}, err
	}
	if existing != nil {
		return *existing, nil
	}
	return createIdeaCloudEpic(ctx, tx, tenantID, projectID)
}

// findIdeaCloudEpic looks up the project's Idea Cloud epic. The lookup is
// deliberately NOT scoped by status: the epic is a durable container, so it must
// be found again after an operator archives it (archiving is a display decision,
// not "delete the anchor") — otherwise the next fire creates a SECOND epic while
// the archived one still holds visible children.
func findIdeaCloudEpic(ctx context.Context, tx pgx.Tx, tenantID, projectID string) (*db.WorkItemRow, error) {
	const q = `SELECT ` + db.WorkItemSelectCols + `
		FROM work_items
		WHERE tenant_id = $1 AND project_id = $2
		  AND kind = $3 AND parent_id IS NULL AND title = $4
		ORDER BY created_at
		LIMIT 1`
	var w db.WorkItemRow
	err := tx.QueryRow(ctx, q, tenantID, projectID,
		domain.WorkItemKindEpic, IdeaCloudEpicTitle).Scan(db.WorkItemScanPtrs(&w)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: find idea cloud epic: %w", err)
	}
	return &w, nil
}

// createIdeaCloudEpic inserts the epic. It is PENDING (not idea): the container
// must be visible in the normal views for a promoted child to resolve its parent.
func createIdeaCloudEpic(ctx context.Context, tx pgx.Tx, tenantID, projectID string) (db.WorkItemRow, error) {
	row := db.WorkItemRow{
		ID:          db.NewID(),
		TenantID:    tenantID,
		ProjectID:   projectID,
		Kind:        domain.WorkItemKindEpic,
		Title:       IdeaCloudEpicTitle,
		Description: IdeaCloudEpicDescription,
		Status:      domain.WorkItemPending,
	}
	created, err := db.CreateWorkItem(ctx, tx, row)
	if err != nil {
		return db.WorkItemRow{}, fmt.Errorf("create idea cloud epic: %w", err)
	}
	return created, nil
}

// hashString is a tiny FNV-1a, used only to derive an advisory-lock key. It does
// not need to be cryptographic — it needs to be stable and cheap.
func hashString(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

// IsIdeaCloudEpic reports whether a row IS the Idea Cloud container. Callers use it
// to exempt the epic from rules that must not apply to it (it is the one epic whose
// children are deliberately idea-state, and it is not itself triageable).
func IsIdeaCloudEpic(kind, title string) bool {
	return kind == domain.WorkItemKindEpic && strings.EqualFold(strings.TrimSpace(title), IdeaCloudEpicTitle)
}

// anchorIdeaToCloudEpic points an idea-state row at the project's Idea Cloud epic,
// making the idea a `task` child of that epic.
//
// THE RULE, and why it is this blunt: every idea is a task under the Idea Cloud
// epic, whatever kind and parent the spawning run proposed. The precedent for not
// trusting a model-asserted identity in a machine-managed path is the ephemeral
// flag ("IDEA landing is forced by the tool, not by parameters"); this is the same
// principle applied to the idea's place in the hierarchy.
//
// A speculatively created STRUCTURAL parent (a pending epic/feature the same run
// invented to hold its ideas) is re-parented under the Idea Cloud epic instead of
// being left behind. Left behind, it is an empty husk carrying automation
// provenance: normal-state, visible in the backlog, and meaningless. Becoming a
// child of the Idea Cloud epic keeps its hierarchy legible. (`parent_id` carries no
// depth-consistency CHECK constraint, and promote is a single status flip, so the
// re-parent cannot fail for an issue/depth reason.)
//
// Idempotent: an idea already parented to the epic is left exactly as it is.
func AnchorIdeaToCloudEpic(ctx context.Context, tx pgx.Tx, row *db.WorkItemRow) error {
	epic, err := ResolveIdeaCloudEpic(ctx, tx, row.TenantID, row.ProjectID)
	if err != nil {
		return err
	}
	if epic.ID == row.ID {
		return nil // the container is never its own child
	}
	// The durable shape: an idea is a task under the container.
	row.Kind = domain.WorkItemKindTask
	spawnedBy := ""
	if row.SpawnedByWorkItemID != nil {
		spawnedBy = *row.SpawnedByWorkItemID
	}
	if row.ParentID != nil && *row.ParentID != "" {
		if *row.ParentID != epic.ID {
			if err := reparentSpeculativeAncestor(ctx, tx, row.TenantID, *row.ParentID, epic.ID, spawnedBy); err != nil {
				return err
			}
		}
	}
	// THE CONTAINER MUST CLAIM THIS FIRE'S EARLIER CHILDREN.
	//
	// A spawning run usually creates a parent FIRST and its ideas under it, so by the
	// time this idea is anchored there are already siblings sitting under the run's own
	// parent — and re-parenting only THIS row would split one fire's output across two
	// containers. Adopting every idea of the same spawn that has not yet been anchored
	// makes the container authoritative for the whole fire, whoever created what first.
	if spawnedBy != "" {
		if err := adoptSpawnSiblings(ctx, tx, epic.ID, row.TenantID, row.ProjectID, spawnedBy); err != nil {
			return err
		}
	}
	epicID := epic.ID
	row.ParentID = &epicID
	return nil
}

// adoptSpawnSiblings re-points every idea-state item of the same automation spawn onto the
// Idea Cloud epic. Scoped to one spawn (spawned_by_work_item_id) and to rows that are
// already idea-state, so it can never touch a human's work or another fire's output.
//
// It runs in the same transaction as the anchoring, so a fire's ideas are never left
// half-adopted.
func adoptSpawnSiblings(ctx context.Context, tx pgx.Tx, epicID, tenantID, projectID, spawnedBy string) error {
	const q = `UPDATE work_items
		SET parent_id = $1, kind = $2, updated_at = now()
		WHERE tenant_id = $3 AND project_id = $4
		  AND spawned_by_work_item_id = $5
		  AND status = $6
		  AND id <> $1
		  AND (parent_id IS NULL OR parent_id <> $1)`
	if _, err := tx.Exec(ctx, q, epicID, domain.WorkItemKindTask,
		tenantID, projectID, spawnedBy, domain.WorkItemIdea); err != nil {
		return fmt.Errorf("adopt spawn siblings: %w", err)
	}
	return nil
}

// reparentSpeculativeAncestor moves a spawn-time parent that the automation run
// invented under the Idea Cloud epic. Only a NON-idea row is touched: an idea-state
// parent is simply left alone (it is triage material itself — the report's epic was
// one, and flattening it would destroy the run's grouping before anyone saw it).
func reparentSpeculativeAncestor(ctx context.Context, tx pgx.Tx, tenantID, ancestorID, epicID, spawnedBy string) error {
	if ancestorID == "" || ancestorID == epicID {
		return nil
	}
	ancestor, err := db.GetWorkItem(ctx, tx, tenantID, ancestorID)
	if errors.Is(err, db.ErrNotFound) {
		return nil // already gone; the new parent supersedes it
	}
	if err != nil {
		return fmt.Errorf("resolve spawn parent: %w", err)
	}
	if ancestor.Status == domain.WorkItemIdea {
		if ancestor.SpawnedByWorkItemID != nil && *ancestor.SpawnedByWorkItemID == spawnedBy {
			// An IDEA-STATE container this same fire invented to group its ideas. It moved
			// every one of its children onto the Idea Cloud epic, so leaving it under its
			// own nothing would strand it as an empty husk — and one named after the
			// subject at hand ("Weatherapp: close the market-capability gap"), which is
			// the confusing artefact the operator sees instead of the one durable
			// "IDEA CLOUD" container. Cancelled rather than deleted: its children keep a
			// resolvable parent, and the cancellation records why it exists.
			reason := `{"cancelled_reason":"superseded by the Idea Cloud epic — its children were re-parented to the durable container"}`
			cancelled := domain.WorkItemCancelled
			results := []byte(reason)
			if _, err := db.UpdateWorkItem(ctx, tx, tenantID, ancestorID, ancestor.Version,
				db.UpdateWorkItemFields{Status: &cancelled, Results: &results}); err != nil {
				return fmt.Errorf("retire speculative container %s: %w", ancestorID, err)
			}
			return nil
		}
		// An idea-state item from ANOTHER fire or a human grouping: not a structural
		// claim, leave it alone.
		return nil
	}
	if ancestor.ParentID != nil && *ancestor.ParentID == epicID {
		return nil // already anchored
	}
	newParent := epicID
	if _, err := db.UpdateWorkItem(ctx, tx, tenantID, ancestorID, ancestor.Version,
		db.UpdateWorkItemFields{ParentID: &newParent}); err != nil {
		return fmt.Errorf("re-parent speculative spawn parent %s: %w", ancestorID, err)
	}
	return nil
}
