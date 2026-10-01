package execution

// worker_skills_test.go — the WORKER-VERSION scope's skill files and inline MCP specs
// (child 7, AC 8c + AC 10).
//
// A worker version's MCP definitions are INLINE: no mcp_servers row is created, so a
// published version stays immutable and the version is what a dispatch pins to. The
// skills are a typed PATH LIST (the TUI's established idiom, the same treatment
// context_files gets) — the deliberate, documented asymmetry with the GUI's file browser.

import (
	"encoding/json"
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// THE VERSION FORM CARRIES BOTH FIELDS, and the free-text `skills` PROMPT SECTION stays
// DISTINCT from the `skill_files` PATH LIST (the label disambiguation, task work item 8).
func TestVersionFieldsCarrySkillFilesAndInlineMCP(t *testing.T) {
	m := newModel(t, &fakePlane{})
	fields := m.versionFields(&apiv1.WorkerVersion{})
	have := map[string]bool{}
	for _, f := range fields {
		have[f.Name] = true
	}
	for _, want := range []string{"skill_files", "mcp_servers"} {
		if !have[want] {
			t.Errorf("versionFields has no %q field — the scope is incomplete", want)
		}
	}
	// The prompt section and the file list must not read as the same thing.
	byName := map[string]string{}
	for _, f := range fields {
		byName[f.Name] = f.Label
	}
	if !strings.Contains(byName["skills"], "prompt text") {
		t.Errorf("the `skills` prompt section is labelled %q — it must say (prompt text) so it "+
			"cannot be read as the file list", byName["skills"])
	}
	if strings.Contains(byName["skill_files"], "prompt text") {
		t.Errorf("the `skill_files` list is labelled %q — it is PATHS, not prose", byName["skill_files"])
	}
}

// A SKILL FILES WRITE REACHES THE API with the paths the operator typed, and PRESERVES the
// version's other fields. This is the same path-list treatment context_files gets.
func TestSkillFilesReachTheUpdateRequest(t *testing.T) {
	req := &apiv1.UpdateWorkerVersionRequest{}
	v := map[string]string{
		"role": "Engineer", "skills": "be careful", "behavior": "x", "agents_md": "y",
		"skill_files": "/a/SKILL.md\n/b", "version_note": "n", "context_sources": "[]",
		"permissions": `{"tools":["read"]}`, "mcp_servers": "[]", "gated_tools": "[]",
		"budget_overrides": "{}", "model_ref": "ollama/llama3",
	}
	setVersionFieldsU(req, v, 2)

	// skill_files is an `optional string` JSON array on the wire.
	got := req.GetSkillFiles()
	if got == "" {
		t.Fatal("the update carries no skill_files — the write would silently no-op")
	}
	var files []string
	if err := json.Unmarshal([]byte(got), &files); err != nil {
		t.Fatalf("skill_files is not a JSON array (%q): %v", got, err)
	}
	if len(files) != 2 || files[0] != "/a/SKILL.md" || files[1] != "/b" {
		t.Fatalf("skill_files = %v, want the two typed paths", files)
	}
	// The other fields survive.
	if req.GetRole() != "Engineer" || req.GetSkills() != "be careful" {
		t.Fatalf("role/skills were dropped: %q / %q", req.GetRole(), req.GetSkills())
	}
}

// AN INLINE MCP SPEC REACHES permissions.mcp_servers — no row is created, and the OTHER
// permissions keys (tools) are preserved: the panel owns ONE key of the blob.
func TestInlineMCPSpecsReachPermissionsWithoutDroppingOtherKeys(t *testing.T) {
	req := &apiv1.UpdateWorkerVersionRequest{}
	v := map[string]string{
		"role": "Engineer", "skills": "s", "behavior": "b", "agents_md": "a",
		"skill_files": "", "version_note": "n", "context_sources": "[]",
		"permissions": `{"tools":["read"]}`,
		"mcp_servers": `[{"id":"github","type":"stdio","command":["npx","-y","x"]}]`,
		"gated_tools": "[]", "budget_overrides": "{}", "model_ref": "",
	}
	setVersionFieldsU(req, v, 2)

	perms := req.GetPermissions()
	var blob map[string]json.RawMessage
	if err := json.Unmarshal([]byte(perms), &blob); err != nil {
		t.Fatalf("permissions is not a JSON object (%q): %v", perms, err)
	}
	if _, ok := blob["tools"]; !ok {
		t.Fatalf("the merge dropped the tools key: %q", perms)
	}
	var specs []struct {
		ID      string   `json:"id"`
		Command []string `json:"command"`
	}
	if err := json.Unmarshal(blob["mcp_servers"], &specs); err != nil {
		t.Fatalf("mcp_servers is not a spec array (%q): %v", string(blob["mcp_servers"]), err)
	}
	if len(specs) != 1 || specs[0].ID != "github" || len(specs[0].Command) != 3 {
		t.Fatalf("mcp_servers specs = %+v, want the one github spec", specs)
	}
}

// A CREATED VERSION CARRIES THE SAME TWO FIELDS (create and update must not diverge).
func TestCreateVersionCarriesSkillFilesAndMCP(t *testing.T) {
	req := &apiv1.CreateWorkerVersionRequest{}
	v := map[string]string{
		"role": "Engineer", "skills": "s", "behavior": "b", "agents_md": "a",
		"skill_files": "/a", "version_note": "n", "context_sources": "[]",
		"permissions": `{"mcp_servers":[{"id":"old","type":"stdio","command":["npx"]}]}`,
		"mcp_servers": `[{"id":"new","type":"stdio","command":["uvx"]}]`,
		"gated_tools": "[]", "budget_overrides": "{}", "model_ref": "",
	}
	setVersionFields(req, v, 2)

	if !strings.Contains(req.GetSkillFiles(), "/a") {
		t.Fatalf("create carries no skill_files: %q", req.GetSkillFiles())
	}
	// The merged spec is the field's, not the blob's stale one.
	if !strings.Contains(req.GetPermissions(), `"new"`) {
		t.Fatalf("create permissions do not carry the edited spec: %q", req.GetPermissions())
	}
}

// AN UNTOUCHED EDIT DOES NOT CHURN: versionUnchanged is FALSE when only skill_files
// changed, and TRUE when nothing did.
func TestVersionUnchangedSeesSkillFilesAndMCP(t *testing.T) {
	src := &apiv1.WorkerVersion{
		ModelRef:    "ollama/llama3",
		Role:        "Engineer",
		Permissions: `{"tools":["read"],"mcp_servers":[{"id":"github","type":"stdio","command":["npx"]}]}`,
	}
	orig := versionSnapshot(src)
	vals := map[string]string{
		"model_ref": src.GetModelRef(), "role": src.GetRole(), "skills": "", "skill_files": "",
		"behavior": "", "agents_md": "", "version_note": "", "context_sources": "",
		"permissions": src.GetPermissions(), "mcp_servers": inlineJSONFromPermissions(src.GetPermissions()),
		"gated_tools": "", "budget_overrides": "",
	}
	if !versionUnchanged(vals, 0, orig) {
		t.Fatal("an untouched form must report unchanged, or every save republishes the version")
	}

	// Only the SKILL FILES changed.
	vals["skill_files"] = "/a/SKILL.md"
	if versionUnchanged(vals, 0, orig) {
		t.Fatal("changing only skill_files must count as a change — otherwise the write is skipped")
	}
	vals["skill_files"] = ""

	// Only the INLINE MCP changed.
	vals["mcp_servers"] = `[{"id":"github","type":"stdio","command":["uvx"]}]`
	if versionUnchanged(vals, 0, orig) {
		t.Fatal("changing only the inline MCP specs must count as a change")
	}
}
