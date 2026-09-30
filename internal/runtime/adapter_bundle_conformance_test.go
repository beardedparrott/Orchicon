package runtime

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/adapter"
)

// TestAdapterConsumesResolvedBundleOrDeclaresWhyNot is the ADAPTER
// BUNDLE-CONFORMANCE gate: every adapter kind the LIVE catalog declares must
// answer BOTH consumption axes in the ONE classification the daemon mounts
// from (classifyAdapter, internal/runtime/bootprofile.go) —
//
//	(a) perSessionBundle — does the kind resolve AND consume the per-session
//	    bundle (project-owned ∪ its own scope)?
//	(b) runUnionAtServe  — does its container-serve path receive the
//	    RUN-LEVEL union at container creation?
//
// It mirrors internal/runtime/adapter_bake_guard_test.go and extends the SAME
// declaration the mount guard reads, so the two can never drift:
//
//	(a) THE KIND LIST IS LIVE, not hand-maintained: it comes from the builtin
//	    provider catalog (adapter.BuiltinAdapterKinds). Every declared kind
//	    must be CLASSIFIED, so a newly declared adapter — codex, or whatever
//	    is next — FAILS this test until it is classified on BOTH axes. A
//	    hand-written list would silently miss it, which is exactly the failure
//	    this gate exists to catch.
//
//	(b) THE CLASSIFICATION IS ASSERTED, NOT TRUSTED: a positive axis must be
//	    backed by consumptionWitness entries whose needles are literally
//	    present in the named file (checked by reading it — the runtime package
//	    cannot import internal/opencode|claude|orchicon). A kind that merely
//	    says "I consume it" with no resolving/baking path wired FAILS. A
//	    negative axis must carry a non-empty note, so silence is a failure.
//	    The gate therefore cannot pass merely because a new kind contributes
//	    nothing.
//
// The failure message is the teacher: its reader is the person adding the
// NEXT adapter, and it names the kind and the precise missing behaviour.
func TestAdapterConsumesResolvedBundleOrDeclaresWhyNot(t *testing.T) {
	kindSet := adapter.BuiltinAdapterKinds()
	if len(kindSet) < 3 {
		t.Fatalf("the builtin provider catalog declared only %d adapter kinds — the LIVE kind source (internal/adapter/providers.go) shrank; this gate derives its coverage from it and would silently under-cover", len(kindSet))
	}
	kinds := make([]string, 0, len(kindSet))
	for k := range kindSet {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)

	// An undeclared kind MUST be reported unclassified: the gate's
	// classification check must be able to FAIL, not vacuously pass. This is
	// the exact non-vacuous-fail demonstration adapter_bake_guard_test.go:64-66
	// uses for the mount axis.
	if _, declared := classifyAdapter("", "codex-unclassified-probe"); declared {
		t.Fatal("an undeclared kind reported declared=true — the classification check is vacuous")
	}

	positives := 0
	witnessesExamined := 0
	for _, kind := range kinds {
		c, declared := classifyAdapter("", kind)
		if declared {
			if c.perSessionBundle {
				positives++
			}
			if c.runUnionAtServe {
				positives++
			}
		}
		failures := conformanceFailures(kind, c, declared, readSourceFile)
		for _, axis := range []struct {
			on        bool
			witnesses []consumptionWitness
		}{{c.perSessionBundle, c.perSessionWitnesses}, {c.runUnionAtServe, c.runUnionWitnesses}} {
			if axis.on {
				witnessesExamined += len(axis.witnesses)
			}
		}
		if len(failures) > 0 {
			t.Fatalf("%s", strings.Join(failures, "\n"))
		}
	}

	// Anti-vacuity: if no kind declared any consumption, the gate would pass
	// trivially. Every builtin kind genuinely consumes the per-session bundle
	// today, so at least len(kinds) positives must exist.
	if positives < len(kinds) {
		t.Fatalf("only %d positive consumption declarations across %d catalog kinds — the gate passes vacuously", positives, len(kinds))
	}
	if witnessesExamined == 0 {
		t.Fatal("no consumption witnesses were examined — every positive axis is declarative only; the gate asserts nothing")
	}
	t.Logf("checked %d catalog kind(s) on both consumption axes; verified %d witness needle(s)", len(kinds), witnessesExamined)
}

// readSourceFile is the production witness reader: it reads a witness file
// path RELATIVE TO this package dir (internal/runtime), so witnesses are
// "../opencode/adapter.go", "../claude/session.go", "../orchicon/mcptools.go",
// "../opencode/servehost.go", and "lifecycle.go".
func readSourceFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// conformanceFailures is the PURE checker (no *testing.T, injectable reader)
// so its fail paths are demonstrable in-test
// (TestAdapterConsumptionCheckerIsNotVacuous). It returns one human teaching
// message per violation, each naming the kind and the precise missing
// behaviour. An empty result means the kind is conformance-clean.
func conformanceFailures(kind string, c adapterClass, declared bool, readFile func(string) ([]byte, error)) []string {
	if !declared {
		return []string{fmt.Sprintf(`adapter kind %q is declared by the builtin provider catalog (internal/adapter/providers.go) but is NOT classified in classifyAdapter (internal/runtime/bootprofile.go). Answer BOTH axes there:
  (a) perSessionBundle  — does it resolve+consume the per-session bundle (project-owned ∪ its own scope)? If yes, set perSessionBundle:true AND add perSessionWitnesses naming the file+needle that PROVES the resolving path is wired (a declaration nothing reads is a comment, not a gate).
  (b) runUnionAtServe   — for a serve-dependent kind (ServeDependent==true) its container serve MUST receive the run-level union at creation (resolveRunUnion → RunMCP): set runUnionAtServe:true with runUnionWitnesses. If it is NOT serve-dependent, set runUnionAtServe:false and a note naming precisely what it does NOT do and why. An unexplained negative is how the NEXT adapter slips past.`, kind)}
	}

	var out []string
	// Axis (a) — per-session bundle.
	if c.perSessionBundle {
		if len(c.perSessionWitnesses) == 0 {
			out = append(out, fmt.Sprintf("adapter kind %q declares perSessionBundle:true but carries NO perSessionWitnesses — the declaration is unasserted (a comment, not a gate). Either add consumptionWitness{file,needle} entries proving the per-session resolving path is wired in the kind's adapter package, or set perSessionBundle:false with a note explaining why it does not consume the per-session bundle.", kind))
		}
		for _, w := range c.perSessionWitnesses {
			if msg := witnessFailure(kind, "perSessionBundle", w, readFile); msg != "" {
				out = append(out, msg)
			}
		}
	} else if strings.TrimSpace(c.note) == "" {
		out = append(out, fmt.Sprintf("adapter kind %q declares perSessionBundle:false with NO note — an unexplained negative. State in the note precisely what the kind does NOT consume and why (e.g. no resolving path, per-serve-process constraint), so the NEXT adapter's author has the answer.", kind))
	}

	// Axis (b) — run-level union at serve creation.
	if c.runUnionAtServe {
		if len(c.runUnionWitnesses) == 0 {
			out = append(out, fmt.Sprintf("adapter kind %q declares runUnionAtServe:true but carries NO runUnionWitnesses — the declaration is unasserted. Add consumptionWitness{file,needle} entries proving the run-level union reaches its serve path at creation (e.g. resolveRunUnion → RunMCP).", kind))
		}
		for _, w := range c.runUnionWitnesses {
			if msg := witnessFailure(kind, "runUnionAtServe", w, readFile); msg != "" {
				out = append(out, msg)
			}
		}
	} else {
		if strings.TrimSpace(c.note) == "" {
			out = append(out, fmt.Sprintf("adapter kind %q declares runUnionAtServe:false with NO note — an unexplained negative. State precisely what it does NOT receive at serve time and why.", kind))
		}
		if serveDependentKind(kind) {
			out = append(out, fmt.Sprintf("adapter kind %q is SERVE-DEPENDENT (ServeDependent==true, internal/runtime/lifecycle.go) yet declares runUnionAtServe:false — a serve-dependent kind MUST receive the run-level union at container creation. Wire it (resolveRunUnion → its serve config) and set runUnionAtServe:true with runUnionWitnesses, or correct ServeDependent if this kind genuinely has no in-container serve.", kind))
		}
	}
	return out
}

// witnessFailure verifies ONE consumptionWitness against the real file. A
// declared axis whose witness file cannot be read, or whose needle is absent,
// FAILS: a stale declaration cannot survive (the file moved / the wiring was
// removed).
func witnessFailure(kind, axis string, w consumptionWitness, readFile func(string) ([]byte, error)) string {
	if strings.TrimSpace(w.file) == "" || strings.TrimSpace(w.needle) == "" {
		return fmt.Sprintf("adapter kind %q axis %s carries an empty witness (file=%q needle=%q) — a witness must name both the file and the needle it proves.", kind, axis, w.file, w.needle)
	}
	body, err := readFile(w.file)
	if err != nil {
		return fmt.Sprintf("adapter kind %q axis %s declares a witness in %q but the file could not be read (%v) — the declaration is stale: the resolving/baking path moved or was removed. Update the witness to the file that now carries it.", kind, axis, w.file, err)
	}
	if !strings.Contains(string(body), w.needle) {
		return fmt.Sprintf("adapter kind %q axis %s declares the wiring needle %q in %q, but that substring is ABSENT — the declaration is stale, or the resolving/baking path was never wired at all. Either wire it (then update the needle) or stop declaring this axis.", kind, axis, w.needle, w.file)
	}
	return ""
}

// readNothing is a witness reader that fails every read, used to prove the
// stale-file fail path fires.
func readNothing(string) ([]byte, error) { return nil, os.ErrNotExist }

// TestAdapterConsumptionCheckerIsNotVacuous proves EVERY fail path of the
// conformance checker actually FIRES — a guard that cannot fail is not a
// guard. Each case below is a violation the gate exists to catch, and each
// must yield a non-empty failure list; the final case proves the checker can
// also PASS a correct real declaration (so it is not a rubber stamp).
func TestAdapterConsumptionCheckerIsNotVacuous(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		c        adapterClass
		declared bool
		read     func(string) ([]byte, error)
	}{
		{
			name:     "unclassified kind fails",
			kind:     "codex",
			declared: false,
			read:     readNothing,
		},
		{
			name:     "positive per-session axis with no witnesses fails",
			kind:     "codex",
			c:        adapterClass{perSessionBundle: true},
			declared: true,
			read:     readNothing,
		},
		{
			name: "positive axis with a bogus needle fails",
			kind: "codex",
			c: adapterClass{
				perSessionBundle:    true,
				perSessionWitnesses: []consumptionWitness{{file: "../opencode/adapter.go", needle: "there-is-no-such-needle"}},
			},
			declared: true,
			read:     readSourceFile,
		},
		{
			name: "positive axis whose witness file cannot be read fails",
			kind: "codex",
			c: adapterClass{
				perSessionBundle:    true,
				perSessionWitnesses: []consumptionWitness{{file: "../opencode/adapter.go", needle: "hostResolvedSet"}},
			},
			declared: true,
			read:     readNothing,
		},
		{
			name:     "both axes negative with no note fails",
			kind:     "codex",
			c:        adapterClass{},
			declared: true,
			read:     readSourceFile,
		},
		{
			name:     "positive run-union axis with no witnesses fails",
			kind:     "codex",
			c:        adapterClass{runUnionAtServe: true, note: "x"},
			declared: true,
			read:     readSourceFile,
		},
		{
			name: "serve-dependent kind declaring runUnionAtServe:false fails",
			kind: adapter.DefaultAdapterKind,
			c: adapterClass{
				perSessionBundle:    true,
				perSessionWitnesses: []consumptionWitness{{file: "../opencode/adapter.go", needle: "hostResolvedSet"}},
				runUnionAtServe:     false,
				note:                "a note is present, but a serve-dependent kind may NOT opt out of the run union",
			},
			declared: true,
			read:     readSourceFile,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := conformanceFailures(tc.kind, tc.c, tc.declared, tc.read); len(got) == 0 {
				t.Fatalf("the checker passed a case the gate exists to catch (%s) — the guard is vacuous", tc.name)
			}
		})
	}

	// The real declaration must PASS: the checker is not a rubber stamp that
	// fails everything.
	for _, kind := range []string{adapter.DefaultAdapterKind, adapter.KindClaude, nativeAdapterKind} {
		c, declared := classifyAdapter("", kind)
		if got := conformanceFailures(kind, c, declared, readSourceFile); len(got) > 0 {
			t.Fatalf("the REAL declaration for kind %q failed the checker — the gate would fail a correct declaration:\n%s", kind, strings.Join(got, "\n"))
		}
	}
}
