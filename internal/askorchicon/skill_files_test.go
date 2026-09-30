package askorchicon

// skill_files_test.go — the Ask half of the shared skills render path (AC2/AC3/AC4).
//
// The same acceptance criterion the scheduler test asserts on the worker side: a skill file selected on a
// project appears in the Ask system prompt TOO, rendered by the SAME platform-side renderer
// (internal/contextfiles.RenderManifest) — one shared code path, no adapter-specific and no Ask-specific
// skill-handling code. This file asserts the union of the conversation's skill_files with its project's, the
// byte-stability of two renders of one selection (the section sits inside the cached static prefix), and the
// render-boundary containment rule.
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/askorchicon/ -run 'SkillManifest|SkillFiles' -v

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// skillConversationForTest creates a project (with a project_dir) and a conversation assigned to it, carrying
// the given conversation-level skill_files JSON. Returns the service, the loaded conversation row, and the
// project dir.
func skillConversationForTest(t *testing.T, dir string, projSkillFiles, convSkillFiles []byte) (*Service, db.ConversationRow, string) {
	t.Helper()
	pool := workItemKindTestPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, workItemKindTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: workItemKindTestTenant, Name: "Ask Skills",
		Slug: "ask-skills-" + strings.ToLower(db.NewID()), Status: "active",
		Goals: []byte("[]"), ProjectDir: dir,
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if len(projSkillFiles) > 0 {
		if _, err := db.UpdateProject(ctx, ttx.Tx, workItemKindTestTenant, proj.ID, proj.Version, db.UpdateProjectFields{
			SkillFiles: &projSkillFiles,
		}); err != nil {
			t.Fatalf("set project skill files: %v", err)
		}
	}
	conv, err := db.CreateConversation(ctx, ttx.Tx, db.ConversationRow{
		ID: db.NewID(), TenantID: workItemKindTestTenant, ProjectID: proj.ID, SkillFiles: convSkillFiles,
	})
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return New(pool, slog.Default(), nil, nil, nil), conv, dir
}

// AC2/AC3 (Ask half): the conversation's skill_files UNION the project's, both rendered by the shared renderer.
func TestSkillManifestSectionUnionsConversationAndProject(t *testing.T) {
	dir := t.TempDir()
	projSkill := filepath.Join(dir, "project-skill.md")
	if err := os.WriteFile(projSkill, []byte("ASK_PROJECT_SKILL\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	convSkill := filepath.Join(dir, "conversation-skill.md")
	if err := os.WriteFile(convSkill, []byte("ASK_CONVERSATION_SKILL\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	svc, conv, _ := skillConversationForTest(t, dir,
		[]byte(`["`+projSkill+`"]`),
		[]byte(`["`+convSkill+`"]`),
	)
	ctx := conversationScopeCtx(conv.ProjectID)

	section := svc.skillManifestSection(ctx, workItemKindTestTenant, conv)
	if !strings.Contains(section, "# Skills") {
		t.Fatalf("the Ask skills section has no `# Skills` heading:\n%s", section)
	}
	if !strings.Contains(section, "ASK_PROJECT_SKILL") {
		t.Fatalf("the PROJECT's skill file did not render into the Ask prompt:\n%s", section)
	}
	if !strings.Contains(section, "ASK_CONVERSATION_SKILL") {
		t.Fatalf("the CONVERSATION's skill file did not render into the Ask prompt:\n%s", section)
	}

	// ...and it reaches the FULL system prompt (this is the acceptance path: the Ask turn's prompt carries it).
	full := buildSystemPrompt(modeIteration, testAgentConfig(), testToolRegistry(), nil, true, nil, "", "", section)
	if !strings.Contains(full, "# Skills") || !strings.Contains(full, "ASK_CONVERSATION_SKILL") {
		t.Fatalf("the full Ask system prompt does not carry the skills section:\n%s", full)
	}
}

// AC4 (Ask half): two renders of one selection are byte-identical.
func TestSkillManifestSectionByteStable(t *testing.T) {
	dir := t.TempDir()
	skillsDir := filepath.Join(dir, "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"zebra.md", "apple.md", "mango.md"} {
		if err := os.WriteFile(filepath.Join(skillsDir, n), []byte("body\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	svc, conv, _ := skillConversationForTest(t, dir, []byte(`["`+skillsDir+`"]`), nil)
	ctx := conversationScopeCtx(conv.ProjectID)

	a := svc.skillManifestSection(ctx, workItemKindTestTenant, conv)
	b := svc.skillManifestSection(ctx, workItemKindTestTenant, conv)
	if a != b {
		t.Fatalf("two Ask skill renders of one selection differ:\n--- a ---\n%s\n--- b ---\n%s", a, b)
	}
	if !strings.Contains(a, "(directory — read on demand)") {
		t.Fatalf("the directory skill did not render as a read-on-demand manifest:\n%s", a)
	}
	// Sorted, not filesystem-dependent.
	ai, mi, zi := strings.Index(a, "apple.md"), strings.Index(a, "mango.md"), strings.Index(a, "zebra.md")
	if !(ai >= 0 && mi >= 0 && zi >= 0 && ai < mi && mi < zi) {
		t.Fatalf("the Ask directory manifest is not sorted (apple=%d mango=%d zebra=%d):\n%s", ai, mi, zi, a)
	}
}

// The render-boundary containment rule on the Ask side: a skill outside the conversation's project dir is
// dropped, not rendered as a dead "could not read" note.
func TestSkillManifestSectionDropsOutOfProjectSkill(t *testing.T) {
	dir := t.TempDir()
	outside, err := os.CreateTemp("", "orchicon-ask-outsider-*.md")
	if err != nil {
		t.Fatal(err)
	}
	outsidePath := outside.Name()
	outside.WriteString("ASK_OUTSIDE_BODY\n")
	outside.Close()
	defer os.Remove(outsidePath)

	svc, conv, _ := skillConversationForTest(t, dir, nil, []byte(`["`+outsidePath+`"]`))
	ctx := conversationScopeCtx(conv.ProjectID)

	section := svc.skillManifestSection(ctx, workItemKindTestTenant, conv)
	if section != "" {
		t.Fatalf("an out-of-project skill must render nothing, got:\n%s", section)
	}
}

// An unassigned conversation with no skills renders nothing (and never fails the turn).
func TestSkillManifestSectionEmptyWhenNoSkills(t *testing.T) {
	svc, conv, _ := skillConversationForTest(t, "", nil, nil)
	ctx := conversationScopeCtx(conv.ProjectID)
	if section := svc.skillManifestSection(ctx, workItemKindTestTenant, conv); section != "" {
		t.Fatalf("expected no skills section, got:\n%s", section)
	}
}

// The RPC accepts an in-project selection and rejects one that leaves the project dir, naming the directory.
func TestSetConversationSkillFilesRPCValidatesWithin(t *testing.T) {
	pool := workItemKindTestPool(t)
	dir := t.TempDir()
	inProject := filepath.Join(dir, "skill.md")
	if err := os.WriteFile(inProject, []byte("s\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A project + assigned conversation, created directly (the RPC only needs the row).
	ctxBg := context.Background()
	ttx, err := pool.BeginTenantTx(ctxBg, workItemKindTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := db.CreateProject(ctxBg, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: workItemKindTestTenant, Name: "RPC Skills",
		Slug: "rpc-skills-" + strings.ToLower(db.NewID()), Status: "active",
		Goals: []byte("[]"), ProjectDir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	conv, err := db.CreateConversation(ctxBg, ttx.Tx, db.ConversationRow{
		ID: db.NewID(), TenantID: workItemKindTestTenant, ProjectID: proj.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctxBg); err != nil {
		t.Fatal(err)
	}
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	svc := New(pool, slog.Default(), nil, nil, nil)

	// In-project ACCEPTED, and the value comes back on the conversation proto.
	resp, err := svc.SetConversationSkillFiles(ctx, connect.NewRequest(&apiv1.SetConversationSkillFilesRequest{
		Id: conv.ID, Files: []string{inProject},
	}))
	if err != nil {
		t.Fatalf("SetConversationSkillFiles (in project): %v", err)
	}
	if got := resp.Msg.Conversation.SkillFiles; len(got) != 1 || got[0] != inProject {
		t.Fatalf("the conversation proto did not carry the skill files: %v", got)
	}

	// Out-of-project REJECTED, with the project dir NAMED.
	_, err = svc.SetConversationSkillFiles(ctx, connect.NewRequest(&apiv1.SetConversationSkillFilesRequest{
		Id: conv.ID, Files: []string{"/etc/passwd"},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("out-of-project skill: err = %v (code %v), want InvalidArgument", err, connect.CodeOf(err))
	}
	if !strings.Contains(err.Error(), dir) {
		t.Fatalf("the rejection must NAME the project dir %q, got: %v", dir, err)
	}

	// An empty list CLEARS the selection.
	cleared, err := svc.SetConversationSkillFiles(ctx, connect.NewRequest(&apiv1.SetConversationSkillFilesRequest{
		Id: conv.ID,
	}))
	if err != nil {
		t.Fatalf("clear skill files: %v", err)
	}
	if len(cleared.Msg.Conversation.SkillFiles) != 0 {
		t.Fatalf("an empty list must clear the selection, got: %v", cleared.Msg.Conversation.SkillFiles)
	}
}
