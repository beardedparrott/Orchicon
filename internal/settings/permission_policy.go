package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/permpolicy"
)

// permission_policy.go: the plane API for the operator's persistent
// permission policy (the deny/accept YAML file).
//
// WHY ON SettingsService AND NOT A SERVICE OF ITS OWN. The policy is
// instance state the operator writes once, exactly like settings; hanging
// it here means both clients reuse the settings client they already have,
// and no new RBAC mapping is needed (rbac.serviceToResource has no
// SettingsService arm, so these RPCs carry today's settings entitlement).
//
// THE FILE IS THE SOURCE OF TRUTH, THE PLANE IS THE SINGLE WRITER. Every
// handler reads the file, mutates the parsed struct, writes it back, and
// returns the REFRESHED policy — a client never patches its own copy, so a
// hand-edit and a GUI change cannot diverge.
//
// No DB transaction here: the mutation is a file write (see WriteFile's
// atomic temp+rename), so the audit row is written in its own short tenant
// tx via recordAuditShort — the same deliberate deviation the backup
// handlers document.

// SetPermissionPolicyPath injects the resolved policy file path (server
// wiring). An empty path falls back to permpolicy.DefaultPath().
func (s *Service) SetPermissionPolicyPath(p string) {
	s.policyPath = p
}

// permissionStore is the ONE accessor, resolved per call: the file is read
// fresh on every request, so an edit made by hand is visible to the very
// next API call (the live-read acceptance criterion).
func (s *Service) permissionStore() *permpolicy.Store {
	path := strings.TrimSpace(s.policyPath)
	if path == "" {
		path = permpolicy.DefaultPath()
	}
	return permpolicy.NewStore(path)
}

// GetPermissionPolicy returns the file path and every entry, deny entries
// first (precedence order).
func (s *Service) GetPermissionPolicy(ctx context.Context, _ *connect.Request[apiv1.GetPermissionPolicyRequest]) (*connect.Response[apiv1.GetPermissionPolicyResponse], error) {
	if _, err := requireTenant(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	store := s.permissionStore()
	p, err := store.Read()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(policyToProto(store.Path, p)), nil
}

// AddPermissionPolicyEntry appends one entry to the named list and returns
// the refreshed policy.
func (s *Service) AddPermissionPolicyEntry(ctx context.Context, req *connect.Request[apiv1.AddPermissionPolicyEntryRequest]) (*connect.Response[apiv1.AddPermissionPolicyEntryResponse], error) {
	if _, err := requireTenant(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	pattern, list, err := entryArgs(req.Msg.GetPattern(), req.Msg.GetList())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	store := s.permissionStore()
	p, err := store.Read()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	before := policyJSON(p)

	// Idempotent: re-adding an entry already present is not an error (a UI
	// that double-submits must not have to special-case it), but it must not
	// either duplicate the rule in the file.
	if !containsEntry(p, list, pattern) {
		if list == permpolicy.ListDeny {
			p.Deny = append(p.Deny, pattern)
		} else {
			p.Accept = append(p.Accept, pattern)
		}
		if err := store.Write(p); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		s.recordAuditShort(ctx, "settings.permission_policy.added", before, policyJSON(p))
	}

	refreshed, err := store.Read()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apiv1.AddPermissionPolicyEntryResponse{
		Policy: policyToProto(store.Path, refreshed),
	}), nil
}

// RemovePermissionPolicyEntry removes one entry from the named list and
// returns the refreshed policy. A missing entry is NotFound (the client is
// telling the operator something that is not there).
func (s *Service) RemovePermissionPolicyEntry(ctx context.Context, req *connect.Request[apiv1.RemovePermissionPolicyEntryRequest]) (*connect.Response[apiv1.RemovePermissionPolicyEntryResponse], error) {
	if _, err := requireTenant(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	pattern, list, err := entryArgs(req.Msg.GetPattern(), req.Msg.GetList())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	store := s.permissionStore()
	p, err := store.Read()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	before := policyJSON(p)
	kept, removed := removeEntry(p, list, pattern)
	if !removed {
		return nil, connect.NewError(connect.CodeNotFound,
			fmt.Errorf("permission policy: no %s entry %q in %s", list, pattern, store.Path))
	}
	if err := store.Write(kept); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	s.recordAuditShort(ctx, "settings.permission_policy.removed", before, policyJSON(kept))

	refreshed, err := store.Read()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apiv1.RemovePermissionPolicyEntryResponse{
		Policy: policyToProto(store.Path, refreshed),
	}), nil
}

// entryArgs normalises and validates the (pattern, list) pair both mutating
// RPCs take. Whitespace is trimmed (an entry with a trailing space would
// match nothing) and an empty pattern is refused rather than written: an
// empty deny entry would be a rule that matches nothing while looking like
// a rule.
func entryArgs(pattern, list string) (string, permpolicy.List, error) {
	pat := strings.TrimSpace(pattern)
	if pat == "" {
		return "", permpolicy.ListDeny, errors.New("permission policy: pattern is required")
	}
	l, err := permpolicy.ParseList(list)
	if err != nil {
		return "", permpolicy.ListDeny, err
	}
	return pat, l, nil
}

func containsEntry(p permpolicy.Policy, list permpolicy.List, pattern string) bool {
	if list == permpolicy.ListDeny {
		return slicesContain(p.Deny, pattern)
	}
	return slicesContain(p.Accept, pattern)
}

// removeEntry returns the policy without (list, pattern) and whether it was
// there.
func removeEntry(p permpolicy.Policy, list permpolicy.List, pattern string) (permpolicy.Policy, bool) {
	out := permpolicy.Policy{Deny: append([]string(nil), p.Deny...), Accept: append([]string(nil), p.Accept...)}
	if list == permpolicy.ListDeny {
		kept, ok := without(out.Deny, pattern)
		out.Deny = kept
		return out, ok
	}
	kept, ok := without(out.Accept, pattern)
	out.Accept = kept
	return out, ok
}

func slicesContain(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func without(xs []string, drop string) ([]string, bool) {
	out := make([]string, 0, len(xs))
	found := false
	for _, x := range xs {
		if x == drop {
			found = true
			continue
		}
		out = append(out, x)
	}
	return out, found
}

// policyToProto renders the policy for the wire, deny first. A deny entry
// is reported overridable=false: that is the UI's cue to say a session
// grant cannot override it, instead of offering a grant the refusal will
// ignore.
func policyToProto(path string, p permpolicy.Policy) *apiv1.GetPermissionPolicyResponse {
	resp := &apiv1.GetPermissionPolicyResponse{
		Path:    path,
		Entries: make([]*apiv1.PermissionPolicyEntry, 0, len(p.Deny)+len(p.Accept)),
	}
	for _, e := range p.Deny {
		resp.Entries = append(resp.Entries, &apiv1.PermissionPolicyEntry{
			Pattern:     e,
			List:        permpolicy.ListDeny.String(),
			Overridable: false,
		})
	}
	for _, e := range p.Accept {
		resp.Entries = append(resp.Entries, &apiv1.PermissionPolicyEntry{
			Pattern:     e,
			List:        permpolicy.ListAccept.String(),
			Overridable: true,
		})
	}
	return resp
}

// policyJSON snapshots the policy for the audit row's before/after.
func policyJSON(p permpolicy.Policy) json.RawMessage {
	b, err := json.Marshal(map[string]any{"deny": p.Deny, "accept": p.Accept})
	if err != nil {
		return nil
	}
	return b
}
