package control

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// The Permissions pane (Control → Permissions) manages the operator's DURABLE
// permission policy through the same three plane RPCs the GUI's Permissions tab
// uses. These tests pin the parts this client owns: the row identity that
// carries (list, pattern), the list-name validation a create form depends on,
// and the detail's statement that a session grant cannot override a deny entry.

func TestPermissionItemIDRoundTripsBothLists(t *testing.T) {
	cases := []struct{ list, pattern string }{
		{"deny", "~/.ssh/**"},
		{"accept", "/srv/shared/**"},
		// A pattern may itself contain the separator and a colon: the list is a
		// fixed prefix, so nothing after it is reinterpreted.
		{"deny", "/weird:path/with:colons/**"},
	}
	for _, c := range cases {
		id := permissionItemID(c.list, c.pattern)
		list, pattern := permissionEntryParts(id)
		if list != c.list || pattern != c.pattern {
			t.Fatalf("round trip %q/%q produced %q/%q", c.list, c.pattern, list, pattern)
		}
	}
	if list, pattern := permissionEntryParts("garbage"); list != "" || pattern != "" {
		t.Fatalf("a foreign row id must not decode, got %q/%q", list, pattern)
	}
}

func TestValidPolicyListRejectsATypo(t *testing.T) {
	for _, ok := range []string{"deny", "deny ", "ACCEPT", "accept"} {
		if err := validPolicyList(ok); err != nil {
			t.Fatalf("%q must validate: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "allow", "Denied", "deny,accept"} {
		if err := validPolicyList(bad); err == nil {
			t.Fatalf("%q must be rejected: a typo must not silently become a deny rule", bad)
		}
	}
}

// A deny entry's detail says the thing the operator needs before reaching for a
// session grant: it cannot override this. An accept entry says the opposite.
func TestPermissionDetailStatesTheGrantRelationship(t *testing.T) {
	m, _ := newWriteModel(t)
	m.SelectSource("permissions")

	for _, tc := range []struct {
		list, pattern string
		wantMsg       string
	}{
		{"deny", "~/.ssh/**", "a session grant CANNOT override this entry"},
		{"accept", "/srv/**", "never asks"},
	} {
		_, fields, _, err := m.detail(context.Background(), "permissions", permissionItemID(tc.list, tc.pattern))
		if err != nil {
			t.Fatalf("detail(%s): %v", tc.list, err)
		}
		joined := ""
		for _, f := range fields {
			joined += f.Key + "=" + f.Value + "\n"
		}
		if !strings.Contains(joined, tc.pattern) {
			t.Fatalf("the entry is not named in the detail:\n%s", joined)
		}
		if !strings.Contains(joined, tc.wantMsg) {
			t.Fatalf("detail for %s must say %q:\n%s", tc.list, tc.wantMsg, joined)
		}
	}
}

// The create form is on the permissions pane and its list field refuses a
// name that is neither deny nor accept (what the form's Validate hook is
// wired to).
func TestNewPermissionFormExistsOnThePermissionsPane(t *testing.T) {
	m, _ := newWriteModel(t)
	m.SelectSource("permissions")
	f := m.newFormForSource()
	if f == nil {
		t.Fatal("the permissions pane has no create form: an entry could only be added by hand-editing the file")
	}
	// newFormForSource is wired per pane; asking another pane must not return
	// the permissions form.
	m.SelectSource("settings")
	if m.newFormForSource() != nil {
		t.Fatal("the permissions form leaked into another pane")
	}
}

// The policy response carries the file path the operator may edit by hand, and
// the pane states it (the "where do I edit this?" question).
func TestPermissionPolicyPathFallsBackWhenUnknown(t *testing.T) {
	if got := permissionPolicyPath(nil); got != "(unknown)" {
		t.Fatalf("nil policy path = %q, want (unknown)", got)
	}
	if got := permissionPolicyPath(&apiv1.GetPermissionPolicyResponse{Path: "/var/lib/orchicon/permission-policy.yaml"}); got != "/var/lib/orchicon/permission-policy.yaml" {
		t.Fatalf("path = %q", got)
	}
}
