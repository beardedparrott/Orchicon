package toolclass

import "testing"

// TestClassify is AC1: the ONE classifier, four classes, over the exact vocabulary the work item
// names. Every row here is a promise both clients inherit.
func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		want Class
	}{
		// Modify — the names that change the work (AC1).
		{"write", Modify},
		{"edit", Modify},
		{"batch_write", Modify},
		{"orchicon_write", Modify},
		{"WRITE", Modify},
		{"  edit  ", Modify},

		// Bash — the shell, counted on its own (AC1).
		{"bash", Bash},
		{"shell", Bash},
		{"orchicon_bash", Bash},

		// Read — the names that inspect the work (AC1).
		{"read", Read},
		{"batch_read", Read},
		{"grep", Read},
		{"batch_grep", Read},
		{"list", Read},
		{"glob", Read},
		{"todoread", Read},

		// Ignore — a card, a decision record, an opaque MCP tool, a name nobody classified (AC2-4).
		{"orchicon_ask_user", Ignore},
		{"ask_user", Ignore},
		{"askuser", Ignore},
		{"permission.allow_once", Ignore},
		{"permission.deny", Ignore},
		{"permission.never_allow", Ignore},
		{"mcp__github__create_issue", Ignore},
		{"mcp__orchicon__create_work_item", Ignore},
		{"orchicon_list_projects", Ignore},
		{"", Ignore},
		{"who_knows", Ignore},
		// todowrite is Ignore DELIBERATELY, and the divergence from orchicon.ConsentReadOnlyTools
		// is a decision rather than an oversight (D7): "reads" on this line means reading the
		// REPOSITORY, and a todo update neither reads the repo nor changes the work.
		{"todowrite", Ignore},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.name); got != c.want {
				t.Errorf("Classify(%q) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

// TestClassifyAskUserIsNeverCounted is AC2 stated as its own assertion: orchicon_ask_user puts a
// CARD on screen, so it must contribute nothing to ANY count. Summed over a ledger of ask_user
// calls alone the line must be empty — the caller appends it to a live line, and a count here
// would claim work that did not happen.
func TestClassifyAskUserNeverCounted(t *testing.T) {
	for _, n := range []string{"orchicon_ask_user", "ask_user", "askuser"} {
		c := Classify(n)
		if c == Modify || c == Read || c == Bash {
			t.Errorf("Classify(%q) = %v: an ask_user is a card, not work", n, c)
		}
	}
}

// TestClassifyPermissionRecordsNeverCounted is AC3: recordPermission (tool_ledger.go:74) appends a
// synthetic "permission.<verdict>" tool call for every consent decision, so an APPROVAL of a write
// must not itself read as a write. The check is on the PREFIX, so every verdict — including ones
// invented later — is covered.
func TestClassifyPermissionRecordsNeverCounted(t *testing.T) {
	for _, n := range []string{
		"permission.allow_once", "permission.allow_always", "permission.deny",
		"permission.never_allow", "permission.policy_error",
	} {
		if c := Classify(n); c != Ignore {
			t.Errorf("Classify(%q) = %v, want Ignore: an approval must not inflate a tool tally", n, c)
		}
	}
}

// TestClassifyUnknownIsIgnore is AC4: a name this package does not recognise is NEVER counted into
// a class it does not belong to. Fails closed, so a new MCP server or a future product tool shows
// up as silence rather than as work nobody did.
func TestClassifyUnknownIsIgnore(t *testing.T) {
	for _, n := range []string{
		"mcp__github__create_issue", "mcp__slack__post_message", "mcp__orchicon__create_work_item",
		"orchicon_brand_new_tool", "some_future_tool", "", "random-name",
	} {
		if c := Classify(n); c == Modify || c == Read || c == Bash {
			t.Errorf("Classify(%q) = %v: an unclassified name must be Ignore, not counted", n, c)
		}
	}
	// Case folding is not "unrecognised": `Write` IS `write`.
	if Classify("Write") != Modify {
		t.Error("Classify(\"Write\") must be Modify: the classifier folds case")
	}
}
