package connection

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/config"
)

// stepModel applies one key to the model and returns the updated value.
func stepModel(m Model, msg tea.Msg) Model {
	next, _ := m.Update(msg)
	if nm, ok := next.(Model); ok {
		return nm
	}
	return m
}

// TestConnectFieldOrderFollowsRenderedLayout pins the 2026-09-10 operator
// bug: in username+password mode the rendered layout is URL → Username →
// Password, so the tab cycle must visit fields in THAT order — never the
// internal array order (URL → credential → username), which landed focus
// on the invisible masked password field and made typing appear dead.
func TestConnectFieldOrderFollowsRenderedLayout(t *testing.T) {
	m := New(nil, ProbeFuncs{})
	m = stepModel(m, tea.KeyMsg{Type: tea.KeyCtrlA}) // password mode
	if !strings.Contains(m.View(), "Username > ") {
		t.Fatalf("password mode never rendered the Username field")
	}
	if m.inputs[m.focus].Prompt != "Server URL > " {
		t.Fatalf("expected URL focused at start, got %q", m.inputs[m.focus].Prompt)
	}
	// Tab once: focus must land on USERNAME (index 2), not the masked
	// password field (index 1).
	m = stepModel(m, tea.KeyMsg{Type: tea.KeyTab})
	if got := m.inputs[m.focus].Prompt; got != "Username > " {
		t.Fatalf("one Tab from URL in password mode focused %q — must be Username (field order must follow the rendered layout)", got)
	}
	m = stepModel(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("opuser")})
	if got := m.inputs[2].Value(); got != "opuser" {
		t.Fatalf("typed username landed in %q (value %q) — must be Username", m.inputs[m.focus].Prompt, got)
	}
	// Tab again: Password.
	m = stepModel(m, tea.KeyMsg{Type: tea.KeyTab})
	if got := m.inputs[m.focus].Prompt; got != "Password > " {
		t.Fatalf("second Tab focused %q — must be Password", got)
	}
	m = stepModel(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hunter2")})
	if got := m.inputs[1].Value(); got != "hunter2" {
		t.Fatalf("typed password landed in wrong field (got %q)", got)
	}
	if m.inputs[1].EchoMode != textinput.EchoPassword {
		t.Fatalf("password field lost its masked echo")
	}
}

// TestConnectNoStaleTokenPrefillInPasswordMode pins the second operator
// bug: a stored oc_… API-key token must never carry into the Password
// field when switching modes (the masked "bunch of asterisks").
func TestConnectNoStaleTokenPrefillInPasswordMode(t *testing.T) {
	profile := &config.Profile{
		URL:        "http://localhost:8080",
		AuthMethod: config.AuthAPIKey,
		Token:      "oc_stale_api_key_value",
		Username:   "orchicon",
	}
	m := New(profile, ProbeFuncs{})
	if got := m.inputs[1].Value(); got != "oc_stale_api_key_value" {
		t.Fatalf("API-key mode should pre-fill the token, got %q", got)
	}
	m = stepModel(m, tea.KeyMsg{Type: tea.KeyCtrlA}) // switch to password mode
	if got := m.inputs[1].Value(); got != "" {
		t.Fatalf("stale API-key token carried into the Password field (%d chars) — must be cleared on mode switch", len(got))
	}
	// Reverse: a stored password token must not leak into API-key mode.
	pwProfile := &config.Profile{
		URL:        "http://localhost:8080",
		AuthMethod: config.AuthPassword,
		Token:      "access-token-secret",
	}
	m2 := New(pwProfile, ProbeFuncs{})
	if got := m2.inputs[1].Value(); got != "access-token-secret" {
		t.Fatalf("password mode should pre-fill its own token, got %q", got)
	}
	m2 = stepModel(m2, tea.KeyMsg{Type: tea.KeyCtrlA}) // switch to API-key mode
	if got := m2.inputs[1].Value(); got != "" {
		t.Fatalf("password token carried into the API-key field (%d chars) — must be cleared", len(got))
	}
}

// TestConnectTabCycleShiftTabReverse pins shift-tab reversing the cycle
// and wrapping from the first rendered field to the last.
func TestConnectTabCycleShiftTabReverse(t *testing.T) {
	m := New(nil, ProbeFuncs{})
	m = stepModel(m, tea.KeyMsg{Type: tea.KeyShiftTab})
	if got := m.inputs[m.focus].Prompt; got != "API key > " {
		t.Fatalf("shift+tab from URL should wrap to the LAST rendered field (API key), got %q", got)
	}
	// And in password mode, shift-tab wraps to Password.
	m2 := New(nil, ProbeFuncs{})
	m2 = stepModel(m2, tea.KeyMsg{Type: tea.KeyCtrlA})
	m2 = stepModel(m2, tea.KeyMsg{Type: tea.KeyShiftTab})
	if got := m2.inputs[m2.focus].Prompt; got != "Password > " {
		t.Fatalf("password-mode shift+tab from URL should wrap to Password, got %q", got)
	}
}