package connection

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/config"
	"github.com/beardedparrott/orchicon/internal/tui/dock"
)

// Rebuilding the profile from the connect form must not drop DISPLAY PREFERENCES.
//
// buildProfile returns a fresh struct — credentials come from the form, which is
// why it is an explicit list rather than a struct copy — but Theme and Newline are
// not credentials, and leaving them out silently discarded the operator's saved
// palette (and their newline chord) for the whole session. Worse, the rebuilt
// profile is what SaveProfile writes back, so the config's stored values were
// scrubbed too. Symptom: "themes are no longer saving in between rebuilds".
func TestBuildProfileCarriesDisplayPreferences(t *testing.T) {
	existing := &config.Profile{
		Name:    "default",
		URL:     "http://old.example",
		Token:   "old-token",
		Theme:   "gruvbox-dark",
		Newline: string(dock.NewlineBoth),
	}
	m := New(existing, ProbeFuncs{})
	m.inputs[fieldURL].SetValue("http://new.example")

	got := m.buildProfile("new-token", "")
	if got.Theme != "gruvbox-dark" {
		t.Errorf("Theme = %q, want it carried through the rebuild", got.Theme)
	}
	if got.Newline != string(dock.NewlineBoth) {
		t.Errorf("Newline = %q, want it carried through the rebuild", got.Newline)
	}
	// Credentials still come from the FORM, not the old profile.
	if got.Token != "new-token" {
		t.Errorf("Token = %q, want the form's value", got.Token)
	}
	if got.URL != "http://new.example" {
		t.Errorf("URL = %q, want the form's value", got.URL)
	}
}

// A first run (no existing profile) must not invent preferences.
func TestBuildProfileWithoutExistingProfile(t *testing.T) {
	m := New(nil, ProbeFuncs{})
	m.inputs[fieldURL].SetValue("http://new.example")
	got := m.buildProfile("tok", "")
	if got.Theme != "" || got.Newline != "" {
		t.Errorf("preferences = (%q, %q), want empty on a first run", got.Theme, got.Newline)
	}
}
