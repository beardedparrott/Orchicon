package diffs

import (
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/testfixtures"
)

// makeEdit builds an apiv1.FileEdit from a fixture vector (only the fields
// GroupByFile reads: path, kind, unified_diff, tool, seq, created_at).
func makeEdit(v testfixtures.FileEditVector, seq int) *apiv1.FileEdit {
	diff := ""
	if v.ExpectedUnifiedDiff != nil {
		diff = *v.ExpectedUnifiedDiff
	}
	return &apiv1.FileEdit{
		Id:          v.Name + "-" + itoa(seq),
		Path:        v.Path,
		Kind:        v.ExpectedKind,
		UnifiedDiff: diff,
		Tool:        "batch_write",
		Seq:         int64(seq),
	}
}

// GroupByFile must group by path and tally adds/dels from the latest edit's
// unified diff — the exact assertions in sideBySide.test.ts.
func TestGroupByFile(t *testing.T) {
	vecs := vecByName(t)
	modify := vecs["modify-two-lines"]
	create := vecs["create-new-file"]

	edits := []*apiv1.FileEdit{makeEdit(create, 1), makeEdit(modify, 2)}
	groups := GroupByFile(edits)
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(groups))
	}
	createGroup := findGroup(groups, create.Path)
	if createGroup == nil {
		t.Fatalf("no group for %s", create.Path)
	}
	if createGroup.Adds != 3 || createGroup.Dels != 0 {
		t.Errorf("create group adds/dels = %d/%d, want 3/0", createGroup.Adds, createGroup.Dels)
	}
	modifyGroup := findGroup(groups, modify.Path)
	if modifyGroup == nil {
		t.Fatalf("no group for %s", modify.Path)
	}
	if modifyGroup.Adds != 1 || modifyGroup.Dels != 1 {
		t.Errorf("modify group adds/dels = %d/%d, want 1/1", modifyGroup.Adds, modifyGroup.Dels)
	}
}

func findGroup(groups []FileGroup, path string) *FileGroup {
	for i := range groups {
		if groups[i].Path == path {
			return &groups[i]
		}
	}
	return nil
}
