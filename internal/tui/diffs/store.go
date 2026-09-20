package diffs

import (
	"context"
	"fmt"
	"sync"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/client"
)

// resolveTenantID resolves the caller's tenant_id once via
// Auth.ListIdentities (the same call the footer's probeIdentity uses) and
// caches it. The FileEditService RPCs REQUIRE a non-empty tenant_id on the
// request message (internal/fileedit/rpc.go → "tenant_id must not be
// empty"), unlike the TUI's project/execution streams which let the plane
// resolve the tenant from the bearer credential. So the diffs pane must
// resolve it explicitly.
type tenantResolver struct {
	cl     *client.Clients
	mu     sync.Mutex
	tenant string
}

func newTenantResolver(cl *client.Clients) *tenantResolver {
	return &tenantResolver{cl: cl}
}

// resolve returns the cached tenant, or resolves it on first call. On
// failure it returns an error ("" + err) so the caller can surface it.
func (r *tenantResolver) resolve(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tenant != "" {
		return r.tenant, nil
	}
	resp, err := r.cl.Auth.ListIdentities(ctx, connect.NewRequest(&apiv1.ListIdentitiesRequest{PageSize: 1}))
	if err != nil {
		return "", fmt.Errorf("resolve tenant via ListIdentities: %w", err)
	}
	if resp == nil || len(resp.Msg.GetIdentities()) == 0 {
		return "", fmt.Errorf("resolve tenant via ListIdentities: no identities returned")
	}
	r.tenant = resp.Msg.GetIdentities()[0].GetTenantId()
	if r.tenant == "" {
		return "", fmt.Errorf("resolve tenant via ListIdentities: identity has no tenant_id")
	}
	return r.tenant, nil
}

// Snapshot is the result of a Fetch: the merged, chronologically-ordered
// edits plus the owner's max durable sequence (the resume point).
type Snapshot struct {
	Edits []*apiv1.FileEdit
	// MaxDurableSeq is the ledger's max seq as of the durable fetch. Live
	// events with seq <= this are dropped by MergeEdits (durable is a
	// superset).
	MaxDurableSeq int64
}

// Store owns the file-edit ledger RPC layer for one pane. It caches the
// tenant, fetches the durable ledger (GetSessionFileEdits), exposes the
// merge helper (MergeEdits), and tracks the owner so the pane can re-fetch
// when the active session changes.
type Store struct {
	cl        *client.Clients
	tenant    *tenantResolver
	mu        sync.Mutex
	ownerKind string
	ownerID   string
}

// NewStore builds a store over the client set.
func NewStore(cl *client.Clients) *Store {
	return &Store{
		cl:     cl,
		tenant: newTenantResolver(cl),
	}
}

// Fetch retrieves the durable file-edit ledger for one owner. It resolves
// the tenant on first use (cached), then calls GetSessionFileEdits. onLive
// is built by the caller (the pane) after Fetch so a running owner gets the
// live StreamFileEdits merged on top.
//
// ownerKind is "execution" or "ask_conversation".
func (s *Store) Fetch(ctx context.Context, ownerKind, ownerID string) (*Snapshot, error) {
	tenant, err := s.tenant.resolve(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.ownerKind, s.ownerID = ownerKind, ownerID
	s.mu.Unlock()

	resp, err := s.cl.FileEdits.GetSessionFileEdits(ctx, connect.NewRequest(&apiv1.GetSessionFileEditsRequest{
		TenantId:  tenant,
		OwnerKind: ownerKind,
		OwnerId:   ownerID,
	}))
	if err != nil {
		return nil, fmt.Errorf("get session file edits: %w", err)
	}
	return &Snapshot{
		Edits:         resp.Msg.GetEdits(),
		MaxDurableSeq: resp.Msg.GetMaxSeq(),
	}, nil
}

// Owner returns the current owner the store is scoped to.
func (s *Store) Owner() (kind, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ownerKind, s.ownerID
}

// Tenant returns the resolved tenant id ("" until first Fetch). Provided so
// the pane can share the same tenant with the live stream subscription.
func (s *Store) Tenant() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tenant.tenant
}

// MergeEdits merges the durable ledger (GetSessionFileEdits) with the live
// stream (StreamFileEdits) using the same discipline as the GUI's
// mergeEdits: the durable fetch is a superset of everything up to ~2s ago,
// so live events are appended only when their sequence exceeds the durable
// max, and deduplicated by id (the stream's event_id == the ledger row id)
// so a reconnect never double-applies. Stable sort ascending by seq.
func MergeEdits(durable, live []*apiv1.FileEdit) []*apiv1.FileEdit {
	byID := map[string]*apiv1.FileEdit{}
	var maxDurableSeq int64
	for _, e := range durable {
		if e == nil {
			continue
		}
		byID[e.GetId()] = e
		if e.GetSeq() > maxDurableSeq {
			maxDurableSeq = e.GetSeq()
		}
	}
	rows := append([]*apiv1.FileEdit{}, durable...)
	for _, e := range live {
		if e == nil {
			continue
		}
		if _, dup := byID[e.GetId()]; dup {
			continue
		}
		if e.GetSeq() <= maxDurableSeq {
			continue
		}
		byID[e.GetId()] = e
		rows = append(rows, e)
		if e.GetSeq() > maxDurableSeq {
			maxDurableSeq = e.GetSeq()
		}
	}
	// Stable sort by seq ascending so the timeline is chronological even if
	// the stream delivered entries slightly out of order across reconnect.
	sortBySeq(rows)
	return rows
}

func sortBySeq(rows []*apiv1.FileEdit) {
	// insertion sort (stable, small n per pane)
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j].GetSeq() < rows[j-1].GetSeq(); j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
}
