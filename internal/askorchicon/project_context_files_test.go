package askorchicon

import (
	"os"
	"strings"
	"testing"
)

// WHY THIS FILE EXISTS.
//
// A project's `context_files` reach every WORKER's prompt (buildCompositePrompt -> readProjectContextFiles
// -> "# Project context"). Ask did NOT inline them: conversationProjectContext NAMES the project
// directory, and skillManifestSection inlines skill_files, so a project's context_files were never in
// an Ask turn.
//
// The operator hit the consequence directly: a context file written so workers know how to reach the
// dev plane reached every worker and no Ask conversation — one project, two populations, one of them
// blind to half the project's context.
//
// This is a source-level assertion because the renderer needs a live project row; the invariant is
// "the Ask prompt path CALLS the shared renderer", which is what would silently regress.
func TestAskPromptInlinesProjectContextFiles(t *testing.T) {
	b, err := os.ReadFile("chat.go")
	if err != nil {
		t.Fatalf("read chat.go: %v", err)
	}
	src := string(b)

	if !strings.Contains(src, "func (s *Service) projectContextFilesSection(") {
		t.Fatal("Ask has no project-context-files section — a project's context_files will reach " +
			"workers and never an Ask conversation")
	}
	// It must use the SAME renderer as the worker path, or the two drift in format and policy.
	if !strings.Contains(src, `contextfiles.RenderManifest("# Project context"`) {
		t.Error("the Ask section does not use the shared contextfiles.RenderManifest(\"# Project context\") " +
			"that the worker path uses")
	}
	// And it must actually be CALLED from the prompt assembly, not merely defined.
	if !strings.Contains(src, "s.projectContextFilesSection(ctx, tenantID, conv)") {
		t.Error("projectContextFilesSection is defined but never called from the prompt assembly — " +
			"a renderer nothing invokes changes nothing")
	}
}
