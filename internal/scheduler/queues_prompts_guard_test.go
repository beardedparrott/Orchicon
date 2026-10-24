package scheduler

// queues_prompts_guard_test.go — EVERY chat adapter must STATE how it handles a mid-turn prompt.
//
// THE BUG THIS EXISTS FOR, and it was found by being asked rather than by testing: "did you cross check
// against the claude code adapter as well to ensure we are handling it properly there?"
//
// No. The first version of SessionQueuesPrompts was declared by the two adapters that happened to be in hand
// (opencode true, the native bridge false) and asked nothing of the other two. An adapter that declares
// nothing falls to the DEFAULT — "does not queue" — which is safe against starting a competing turn but WRONG
// for function:
//
//   - internal/claude's Bridge ALREADY QUEUES. Its session increments a `queued` counter, writes the turn to
//     the live child's stdin, and deliberately keeps the run loop alive past the `result` boundary because "a
//     mid-run injected turn is still outstanding" (session.go). Falling to the default meant an interjection on
//     a claude conversation was held until the WHOLE turn finished instead of being folded in at the next
//     boundary — a silent downgrade of exactly the behaviour the operator asked for.
//   - internal/opencode's ClientSessionAdapter sends through the same serve via prompt_async, so it queues
//     too, and it was ALSO falling to the default.
//
// A DEFAULT IS NOT A CONTRACT. This test walks the repository, finds every type that claims to implement
// ChatTurnClient, and requires each of them to declare QueuesPrompts explicitly. A new adapter cannot be
// added without answering the question, and an existing one cannot quietly lose its answer.

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// chatClientAssertion matches a compile-time interface assertion such as
//
//	var _ scheduler.ChatTurnClient = (*Adapter)(nil)
//	_ scheduler.ChatTurnClient     = (*Bridge)(nil)
var chatClientAssertion = regexp.MustCompile(`(?m)(?:_|var\s+_)\s+(?:\w+\.)?ChatTurnClient\s*=\s*\(\*?(\w+)\)\(nil\)`)

// TestEveryChatAdapterDeclaresHowItQueues is the guard.
func TestEveryChatAdapterDeclaresHowItQueues(t *testing.T) {
	root := filepath.Join("..") // internal/
	type impl struct {
		dir  string
		file string
		typ  string
	}
	var impls []impl

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "testdata" || name == "node_modules" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, m := range chatClientAssertion.FindAllStringSubmatch(string(b), -1) {
			impls = append(impls, impl{dir: filepath.Dir(path), file: filepath.Base(path), typ: m[1]})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	// VACUITY: if the walk found no adapters, the guard is not looking at anything.
	if len(impls) < 3 {
		t.Fatalf("found only %d ChatTurnClient implementation(s) — the walk is not reaching the adapters", len(impls))
	}

	for _, im := range impls {
		// The declaration must exist ON THIS TYPE, in the same package — not merely somewhere in the repo.
		decl := regexp.MustCompile(`func \([a-zA-Z0-9_]+ \*?` + regexp.QuoteMeta(im.typ) + `\) QueuesPrompts\(\) bool`)
		found := false
		entries, derr := os.ReadDir(im.dir)
		if derr != nil {
			t.Fatalf("read %s: %v", im.dir, derr)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
				continue
			}
			b, rerr := os.ReadFile(filepath.Join(im.dir, e.Name()))
			if rerr != nil {
				continue
			}
			if decl.MatchString(string(b)) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s/%s implements ChatTurnClient but does not declare QueuesPrompts() on %s.\n"+
				"  Declaring it is not bookkeeping: it decides HOW an interjection is delivered. Say true if\n"+
				"  SendTurnMessage on a live session QUEUES the message for the running turn's next boundary\n"+
				"  (opencode's prompt_async, claude's stdin write), or false if it starts its own turn (the native\n"+
				"  bridge). Falling to the default silently downgrades interjections for that adapter.",
				im.dir, im.file, im.typ)
		}
	}
}
