package db

// conversation_project_test.go — THE CONVERSATION COLUMN LIST CANNOT DRIFT FROM ITS SCAN.

import (
	"strings"
	"testing"
)

// THE PROJECT COLUMN IS IN THE SHARED LIST. If it were dropped, every conversation would come back unassigned
// and both clients' grouping would silently flatten.
func TestConversationColumnsIncludeTheProject(t *testing.T) {
	found := false
	for _, c := range conversationCols {
		if c == "project_id" {
			found = true
		}
	}
	if !found {
		t.Errorf("conversationCols has no project_id: %v", conversationCols)
	}
}

// AND THE QUALIFIED FORM HAS THE SAME COLUMNS IN THE SAME ORDER.
func TestTheQualifiedColumnListMatchesThePlainOne(t *testing.T) {
	plain := strings.Split(conversationSelect(""), ", ")
	qualified := strings.Split(conversationSelect("c"), ", ")
	if len(plain) != len(qualified) {
		t.Fatalf("plain has %d columns, qualified has %d — the two renderings disagree", len(plain), len(qualified))
	}
	if len(plain) != len(conversationCols) {
		t.Fatalf("rendered %d columns from a %d-entry list", len(plain), len(conversationCols))
	}
	for i, c := range qualified {
		if !strings.HasPrefix(c, "c.") {
			t.Errorf("qualified column %d is %q, which carries no table alias", i, c)
		}
		if base := strings.TrimPrefix(c, "c."); base != plain[i] {
			t.Errorf("column %d is %q qualified but %q plain — the order or the name differs", i, base, plain[i])
		}
	}
}
