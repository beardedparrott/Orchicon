package settings

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/permpolicy"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

const testPolicy = `deny:
  - ~/.ssh/**
accept:
  - /srv/shared/**
`

// newPolicyService builds a Service wired to a temp policy file. The pool is
// nil on purpose: the policy lives in the state dir, not Postgres, and every
// path exercised here is the file (plus, for the mutating RPCs, an audit row
// that is best-effort by construction).
func newPolicyService(t *testing.T, body string) (*Service, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "permission-policy.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	s := &Service{log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	s.SetPermissionPolicyPath(path)
	return s, path
}

func tenantCtx() context.Context { return tenant.WithID(context.Background(), "tnt_test") }

// The list RPC both clients call: deny entries first, and a deny entry is
// reported NOT overridable (the cue a UI needs to say a session grant cannot
// override it).
func TestGetPermissionPolicyListsEntriesAndMarksDenyNotOverridable(t *testing.T) {
	s, path := newPolicyService(t, testPolicy)

	resp, err := s.GetPermissionPolicy(tenantCtx(), connect.NewRequest(&apiv1.GetPermissionPolicyRequest{}))
	if err != nil {
		t.Fatalf("GetPermissionPolicy: %v", err)
	}
	if resp.Msg.GetPath() != path {
		t.Fatalf("path = %q, want %q", resp.Msg.GetPath(), path)
	}
	entries := resp.Msg.GetEntries()
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2 (%+v)", len(entries), entries)
	}
	if entries[0].GetPattern() != "~/.ssh/**" || entries[0].GetList() != "deny" {
		t.Fatalf("deny entry first, got %+v", entries[0])
	}
	if entries[0].GetOverridable() {
		t.Fatal("a deny entry must report overridable=false: a session grant cannot override it")
	}
	if entries[1].GetPattern() != "/srv/shared/**" || entries[1].GetList() != "accept" {
		t.Fatalf("accept entry second, got %+v", entries[1])
	}
	if !entries[1].GetOverridable() {
		t.Fatal("an accept entry is overridable")
	}
}

// A hand-edit of the file is visible to the very next API call: the API is a
// reader of the FILE, not the owner of a copy. This is the property that
// makes a GUI change and a hand-edit the same change.
func TestGetPermissionPolicySeesAHandEditImmediately(t *testing.T) {
	s, path := newPolicyService(t, testPolicy)

	if err := os.WriteFile(path, []byte("deny: []\naccept: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resp, err := s.GetPermissionPolicy(tenantCtx(), connect.NewRequest(&apiv1.GetPermissionPolicyRequest{}))
	if err != nil {
		t.Fatalf("GetPermissionPolicy: %v", err)
	}
	if n := len(resp.Msg.GetEntries()); n != 0 {
		t.Fatalf("entries = %d, want 0 after the hand-edit", n)
	}
}

// The write path a client uses (read → mutate → write through the SAME store
// the handlers use) is visible to a reader that only knows the file — the
// TUI's and the GUI's read. And the enforcement side (the shared accessor)
// agrees with it.
func TestAddedEntryIsVisibleToTheReaderAndToEnforcement(t *testing.T) {
	s, path := newPolicyService(t, "deny: []\naccept: []\n")
	store := s.permissionStore()

	p, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	p.Deny = append(p.Deny, "/srv/secret/**")
	if err := store.Write(p); err != nil {
		t.Fatalf("write: %v", err)
	}

	// A reader that navigated the GUI path (Get) sees it...
	resp, err := s.GetPermissionPolicy(tenantCtx(), connect.NewRequest(&apiv1.GetPermissionPolicyRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.GetEntries()) != 1 || resp.Msg.GetEntries()[0].GetPattern() != "/srv/secret/**" {
		t.Fatalf("reader did not see the added entry: %+v", resp.Msg.GetEntries())
	}
	// ...and so does a completely independent reader of the file (a TUI
	// process, a hand-edit, the boot check).
	fresh := permpolicy.NewStore(path)
	fp, err := fresh.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(fp.Deny) != 1 || fp.Deny[0] != "/srv/secret/**" {
		t.Fatalf("a fresh reader of the file did not see the entry: %+v", fp)
	}
	// ...and ENFORCEMENT now refuses the path: the client surface and the
	// guard read one file, so they cannot disagree.
	d, err := fresh.Decide("/srv/secret/key.pem", permpolicy.Inputs{SessionGranted: true})
	if err != nil {
		t.Fatal(err)
	}
	if d.Verdict != permpolicy.VerdictDeny || d.Entry != "/srv/secret/**" {
		t.Fatalf("enforcement disagrees with the client surface: %+v", d)
	}
}

func TestEntryArgsValidation(t *testing.T) {
	if _, _, err := entryArgs("   ", "deny"); err == nil {
		t.Fatal("an empty pattern must be refused, not written")
	}
	if _, _, err := entryArgs("/x", "allow"); err == nil {
		t.Fatal("an unknown list name must be refused")
	}
	pat, list, err := entryArgs("  /x/**  ", "accept")
	if err != nil || pat != "/x/**" || list != permpolicy.ListAccept {
		t.Fatalf("got (%q,%v,%v)", pat, list, err)
	}
}

func TestRemoveEntryRemovesExactlyOneList(t *testing.T) {
	in := permpolicy.Policy{Deny: []string{"a", "b"}, Accept: []string{"b"}}
	out, ok := removeEntry(in, permpolicy.ListDeny, "b")
	if !ok {
		t.Fatal("expected the deny entry to be found")
	}
	if strings.Join(out.Deny, ",") != "a" {
		t.Fatalf("deny = %v, want [a]", out.Deny)
	}
	if strings.Join(out.Accept, ",") != "b" {
		t.Fatalf("the accept list must be untouched, got %v", out.Accept)
	}
	if _, ok := removeEntry(in, permpolicy.ListAccept, "zzz"); ok {
		t.Fatal("a missing entry must report not-found (NotFound on the wire)")
	}
	// The input policy is never mutated in place.
	if strings.Join(in.Deny, ",") != "a,b" {
		t.Fatalf("removeEntry mutated its input: %v", in.Deny)
	}
}
