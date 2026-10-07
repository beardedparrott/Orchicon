package control

// provider_note_test.go — both provider forms tell the operator what a LOCAL model URL
// means for a runtime container, and what it needs to work.
//
// The operator: "I also think we need to modify the add and edit provider sections in the
// GUI and TUI to give a hint that a firewall rule will need to be added for the docker
// container IP to make their local models work with both claude and opencode. Otherwise
// users would be lost."
//
// Without it the failure is genuinely undiagnosable: Ask Orchicon works (the plane is on
// the host and dials 127.0.0.1 directly) while container workers fail silently on a
// blocked bridge port — one address, two consumers, only one of them broken.

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

func TestProviderFormsCarryTheLocalModelNote(t *testing.T) {
	for _, sz := range [][2]int{{140, 40}, {120, 34}, {100, 30}} {
		m, _ := newWriteModel(t)
		m.SetSize(sz[0], sz[1])
		f := m.newProviderForm()
		f.Width = 64
		v := ansi.Strip(f.View())
		has := strings.Contains(v, "FIREWALL RULE") && strings.Contains(v, "172.17.0.1")
		t.Logf("=== %dx%d: note present=%v ===", sz[0], sz[1], has)
		for _, l := range strings.Split(v, "\n") {
			if strings.Contains(l, "Local models need") || strings.Contains(l, "FIREWALL") || strings.Contains(l, "bridge") || strings.Contains(l, "localhost is itself") {
				t.Logf("| %s", strings.TrimRight(l, " "))
			}
		}
		if !has {
			t.Fatalf("the provider form must carry the address+firewall note")
		}
	}
	// And the EDIT form carries it too — the two must not drift.
	m2, _ := newWriteModel(t)
	m2.Base.LoadItems("providers", []kit2.Item{{ID: "local-gufo", Title: "Gufo"}}, "")
	m2.Base.SelectItem("providers", "local-gufo")
	it, ok := m2.ActiveItem()
	if !ok {
		t.Fatal("fixture: no provider row selected")
	}
	ef := m2.editProviderForm(it)
	if !strings.Contains(ansi.Strip(ef.View()), "FIREWALL RULE") {
		t.Fatal("the EDIT provider form must carry the same note")
	}
	t.Logf("edit form carries the note too")
}
