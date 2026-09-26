package permpolicy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePolicy writes body to a temp policy file and returns a Store for it.
func writePolicy(t *testing.T, body string) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "permission-policy.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	return NewStore(path)
}

// AC: a preset deny entry (`~/.ssh/**`) blocks a write even after a session
// grant, and the refusal names the entry.
func TestPresetDenyOutranksSessionGrantAndNamesTheEntry(t *testing.T) {
	s := writePolicy(t, PresetYAML)

	d, err := s.Decide("~/.ssh/id_rsa", Inputs{SessionGranted: true})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if d.Verdict != VerdictDeny {
		t.Fatalf("verdict = %v, want deny", d.Verdict)
	}
	if d.Entry != "~/.ssh/**" {
		t.Fatalf("entry = %q, want %q (the refusal must name the entry)", d.Entry, "~/.ssh/**")
	}

	// The same decision with EVERY louder input set: a grant and the
	// conversation's own project. Deny is still the answer.
	d2, err := s.Decide("~/.ssh/id_rsa", Inputs{SessionGranted: true})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if d2.Verdict != VerdictDeny {
		t.Fatalf("verdict with grant+project = %v, want deny", d2.Verdict)
	}

	// The refusal the suite hands the model names the entry and says a grant
	// cannot override it.
	err = s.HostSuiteGuard()([]string{"~/.ssh/id_rsa"})
	if err == nil {
		t.Fatal("host-suite guard allowed a denied path")
	}
	msg := err.Error()
	for _, want := range []string{"~/.ssh/id_rsa", `"~/.ssh/**"`, s.Path, "cannot override"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal %q missing %q", msg, want)
		}
	}
}

// AC: deleting the entry and making the same call again succeeds without
// restarting the plane — one live Store, no re-construction, no cache.
func TestDeleteEntryTakesEffectOnTheNextCallWithoutRestart(t *testing.T) {
	s := writePolicy(t, PresetYAML)

	if d, err := s.Decide("~/.ssh/id_rsa", Inputs{}); err != nil || d.Verdict != VerdictDeny {
		t.Fatalf("before delete: verdict=%v err=%v, want deny", d.Verdict, err)
	}

	// The operator deletes the entry (here: a rewrite through the SAME store
	// the plane uses; a hand-edit is byte-identical to this).
	if err := s.Write(Policy{Deny: []string{"~/.aws/**"}, Accept: nil}); err != nil {
		t.Fatalf("write: %v", err)
	}

	d, err := s.Decide("~/.ssh/id_rsa", Inputs{})
	if err != nil {
		t.Fatalf("after delete: %v", err)
	}
	if d.Verdict == VerdictDeny {
		t.Fatal("deny survived the edit — the policy file is being cached")
	}
	if d.Verdict != VerdictAsk {
		t.Fatalf("verdict = %v, want ask (no policy covers it any more)", d.Verdict)
	}
	if err := s.HostSuiteGuard()([]string{"~/.ssh/id_rsa"}); err != nil {
		t.Fatalf("host-suite guard still refused after the entry was deleted: %v", err)
	}
	// The sibling entry survived.
	if d, _ := s.Decide("~/.aws/credentials", Inputs{}); d.Verdict != VerdictDeny {
		t.Fatalf("sibling deny entry lost: %v", d.Verdict)
	}
}

// AC: an accept entry never raises a consent ask.
func TestAcceptEntryNeverAsks(t *testing.T) {
	s := writePolicy(t, "deny: []\naccept:\n  - /srv/shared/**\n")
	d, err := s.Decide("/srv/shared/report.csv", Inputs{})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if d.Verdict != VerdictAccept {
		t.Fatalf("verdict = %v, want accept (never an ask)", d.Verdict)
	}
	if d.Entry != "/srv/shared/**" {
		t.Fatalf("entry = %q", d.Entry)
	}
	if !d.Verdict.Proceed() {
		t.Fatal("accept verdict must proceed silently")
	}
}

// AC: precedence is asserted by test — deny > session grant > accept > ask, in
// that order.
//
// THERE IS NO PROJECT RUNG ANY MORE. The conversation's own project used to sit
// between accept and ask as a pre-approved default scope; the operator removed it
// ("any directory should ask before allowing on write/execute, project or
// otherwise"). What matters for the chain is that accepting the project as a
// scope no longer short-circuits anything: a bare path with no grant and no accept
// entry ASKS, project or not.
func TestPrecedenceOrder(t *testing.T) {
	// deny wins over grant AND accept.
	s := writePolicy(t, "deny:\n  - /srv/secret/**\naccept:\n  - /srv/secret/**\n")
	if d, _ := s.Decide("/srv/secret/key.pem", Inputs{SessionGranted: true}); d.Verdict != VerdictDeny {
		t.Fatalf("deny must outrank grant/accept, got %v", d.Verdict)
	}

	// grant wins over accept (both silently proceed, but the grant is the reason
	// — pinned so the order cannot be shuffled).
	s2 := writePolicy(t, "deny: []\naccept:\n  - /srv/**\n")
	if d, _ := s2.Decide("/srv/x", Inputs{SessionGranted: true}); d.Verdict != VerdictGrant {
		t.Fatalf("grant must outrank accept, got %v", d.Verdict)
	}

	// accept wins over ask.
	if d, _ := s2.Decide("/srv/x", Inputs{}); d.Verdict != VerdictAccept {
		t.Fatalf("accept must outrank ask, got %v", d.Verdict)
	}

	// NOTHING CATCHES A PATH THAT IS ONLY "IN THE PROJECT": it asks, exactly as
	// any other unconfigured path does.
	s3 := writePolicy(t, "deny: []\naccept: []\n")
	if d, _ := s3.Decide("/proj/a.go", Inputs{}); d.Verdict != VerdictAsk {
		t.Fatalf("an in-project path with no grant and no accept entry must ASK, got %v", d.Verdict)
	}
	// and nothing at all ⇒ ask.
	if d, _ := s3.Decide("/elsewhere/a.go", Inputs{}); d.Verdict != VerdictAsk {
		t.Fatalf("want ask, got %v", d.Verdict)
	}
}

// AC: an empty or absent file means "no policy" (nothing denied, nothing
// pre-accepted) — never "deny everything".
func TestEmptyOrAbsentMeansNoPolicy(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	s := NewStore(absent)
	if p, err := s.Read(); err != nil || !p.Empty() {
		t.Fatalf("absent file: policy=%+v err=%v, want empty policy and no error", p, err)
	}
	if d, err := s.Decide("~/.ssh/id_rsa", Inputs{}); err != nil || d.Verdict != VerdictAsk {
		t.Fatalf("absent file must not deny: verdict=%v err=%v", d.Verdict, err)
	}
	if err := s.HostSuiteGuard()([]string{"~/.ssh/id_rsa"}); err != nil {
		t.Fatalf("absent file must not refuse: %v", err)
	}

	for _, body := range []string{"", "\n", "deny: []\naccept: []\n", "# just a comment\n"} {
		es := writePolicy(t, body)
		if d, err := es.Decide("~/.ssh/id_rsa", Inputs{}); err != nil || d.Verdict != VerdictAsk {
			t.Fatalf("empty file %q must mean no policy, got verdict=%v err=%v", body, d.Verdict, err)
		}
	}
}

// AC: a malformed file fails loudly (path + parse error) and never silently
// allows everything.
func TestMalformedFileFailsLoudAndNeverAllows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "permission-policy.yaml")
	if err := os.WriteFile(path, []byte("deny: [\n  this is not yaml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("malformed policy loaded without error")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error must name the path: %v", err)
	}
	// Boot must fail too (the plane refuses to start).
	if err := Boot(path); err == nil {
		t.Fatal("Boot accepted a malformed policy")
	}
	// And a consult must not proceed: it returns the error, it does not fall
	// through to a permissive verdict.
	if _, err := NewStore(path).Decide("~/.ssh/id_rsa", Inputs{}); err == nil {
		t.Fatal("consult on a malformed policy must error, not proceed")
	}
	if err := NewStore(path).HostSuiteGuard()([]string{"~/.ssh/id_rsa"}); err == nil {
		t.Fatal("host-suite guard must fail closed on a malformed policy")
	}

	// Unknown keys are malformed too.
	unknown := filepath.Join(t.TempDir(), "permission-policy.yaml")
	if err := os.WriteFile(unknown, []byte("denny:\n  - ~/.ssh/**\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(unknown); err == nil {
		t.Fatal("unknown key must be an error (a typo'd deny list must not read as 'no policy')")
	}
}

// A write round-trips through the file, drops the commented header in, and
// keeps the mode tight.
func TestWriteRoundTripsAndIsMode0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "permission-policy.yaml")
	s := NewStore(path)
	if err := s.Write(Policy{Deny: []string{"~/.ssh/**"}, Accept: []string{"/srv/ok"}}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := s.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got.Deny) != 1 || got.Deny[0] != "~/.ssh/**" || len(got.Accept) != 1 || got.Accept[0] != "/srv/ok" {
		t.Fatalf("round trip lost entries: %+v", got)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), "# Orchicon permission policy") {
		t.Fatalf("written file lost its documented header:\n%s", raw)
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("write left scratch files behind: %v", entries)
	}
}

// The preset (what a default install ships) parses and denies exactly the
// credential stores, and pre-accepts nothing.
func TestPresetContent(t *testing.T) {
	p := MustParsePreset()
	if len(p.Accept) != 0 {
		t.Fatalf("preset must pre-accept nothing, got %v", p.Accept)
	}
	if len(p.Deny) != 7 {
		t.Fatalf("preset deny = %v, want the 7 documented entries", p.Deny)
	}
	// Every preset entry already parses in PresetYAML's own text, so a
	// hand-written file with the shipped block behaves identically.
	if loaded, err := Parse("<preset>", []byte(PresetYAML)); err != nil || len(loaded.Deny) != 7 {
		t.Fatalf("PresetYAML must parse to 7 deny entries: %+v err=%v", loaded, err)
	}
}

// ~ expands for matching, and the deny match also covers the directory
// itself (`~/.ssh/**` covers `~/.ssh`).
func TestTildeExpansionAndDirectoryItself(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home dir")
	}
	s := writePolicy(t, PresetYAML)
	for _, target := range []string{"~/.ssh/id_rsa", filepath.Join(home, ".ssh", "config"), "~/.ssh", "$HOME/.ssh/id_rsa"} {
		if d, err := s.Decide(target, Inputs{}); err != nil || d.Verdict != VerdictDeny {
			t.Fatalf("%s: verdict=%v err=%v, want deny", target, d.Verdict, err)
		}
	}
	// A sibling that merely shares a prefix is not covered.
	if d, _ := s.Decide("~/.sshrc-thing/notes", Inputs{}); d.Verdict == VerdictDeny {
		t.Fatal("a prefix-sharing sibling must not match ~/.ssh/**")
	}
}

func TestParseListRejectsUnknown(t *testing.T) {
	if _, err := ParseList("deny "); err != nil {
		t.Fatalf("deny: %v", err)
	}
	if _, err := ParseList("accept"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if _, err := ParseList("allow"); err == nil {
		t.Fatal("unknown list name must error, not default")
	}
}

func TestDefaultPathHonoursEnv(t *testing.T) {
	t.Setenv(PolicyEnv, "/tmp/explicit.yaml")
	if got := DefaultPath(); got != "/tmp/explicit.yaml" {
		t.Fatalf("DefaultPath = %q, want the explicit env value", got)
	}
	if !ExplicitPath() {
		t.Fatal("ExplicitPath must be true when the env var is set")
	}
}
