package mcpforms

// mcpforms_test.go — the ONE TUI MCP surface's contract (child 7, AC 9 + AC 3 + AC 10).
//
// What is pinned here is the DEFINE path (not the removed select-from-tenant field),
// the TWO persistence targets (owned rows vs inline specs), and the parsers that let a
// legacy worker version round-trip through the same control.

import (
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// kit2Form is the kit2 form type, aliased so the helper's signature reads locally.
type kit2Form = kit2.Form

// DEFINEFORM STAMPS THE OWNER: the owner column IS the selection, so a create with
// neither field set would be a tenant-level entry, which no longer exists.
func TestDefineFormStampsTheOwner(t *testing.T) {
	for _, tc := range []struct {
		name  string
		owner Owner
		check func(t *testing.T, r *apiv1.MCPServerCreateRequest)
	}{
		{
			name:  "project",
			owner: Owner{ProjectID: "p1"},
			check: func(t *testing.T, r *apiv1.MCPServerCreateRequest) {
				if r.GetProjectId() != "p1" || r.GetConversationId() != "" {
					t.Fatalf("owner = project %q / conversation %q, want p1 / empty (XOR)",
						r.GetProjectId(), r.GetConversationId())
				}
			},
		},
		{
			name:  "conversation",
			owner: Owner{ConversationID: "c1"},
			check: func(t *testing.T, r *apiv1.MCPServerCreateRequest) {
				if r.GetConversationId() != "c1" || r.GetProjectId() != "" {
					t.Fatalf("owner = conversation %q / project %q, want c1 / empty (XOR)",
						r.GetConversationId(), r.GetProjectId())
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got *apiv1.MCPServerCreateRequest
			f := DefineForm("Define", tc.owner, nil, func(r *apiv1.MCPServerCreateRequest) tea.Cmd {
				got = r
				return nil
			})
			f.Set("name", "github")
			submitForm(t, f)
			if got == nil {
				t.Fatal("submit produced no request")
			}
			tc.check(t, got)
		})
	}
}

// DEFINE builds a real stdio create: command/args/env, with ${SECRET_NAME} preserved
// verbatim (resolved by the tenant secrets store at session time, never here).
func TestDefineFormBuildsAStdioRequest(t *testing.T) {
	var got *apiv1.MCPServerCreateRequest
	f := DefineForm("Define", Owner{ProjectID: "p1"}, nil, func(r *apiv1.MCPServerCreateRequest) tea.Cmd {
		got = r
		return nil
	})
	f.Set("name", "github")
	f.Set("transport", "stdio")
	f.Set("command", "npx")
	f.Set("args", "-y @modelcontextprotocol/server-github")
	f.Set("env", "GITHUB_TOKEN=${GITHUB_TOKEN}")
	submitForm(t, f)

	if got == nil {
		t.Fatal("submit produced no request")
	}
	if got.GetName() != "github" || got.GetProjectId() != "p1" {
		t.Fatalf("request = %+v, want github owned by p1", got)
	}
	if got.GetCommand() != "npx" || len(got.GetArgs()) != 2 || got.GetArgs()[0] != "-y" {
		t.Fatalf("command/args = %q/%v", got.GetCommand(), got.GetArgs())
	}
	if got.GetTransport() != apiv1.MCPServerTransport_MCP_SERVER_TRANSPORT_STDIO {
		t.Fatalf("transport = %v, want STDIO", got.GetTransport())
	}
	if got.GetEnv()["GITHUB_TOKEN"] != "${GITHUB_TOKEN}" {
		t.Fatalf("env = %v, want the ${SECRET_NAME} reference preserved", got.GetEnv())
	}
}

// The HTTP branch writes url/headers, and still preserves ${SECRET_NAME}.
func TestDefineFormBuildsAStreamableHTTPRequest(t *testing.T) {
	var got *apiv1.MCPServerCreateRequest
	f := DefineForm("Define", Owner{ConversationID: "c1"}, nil, func(r *apiv1.MCPServerCreateRequest) tea.Cmd {
		got = r
		return nil
	})
	f.Set("name", "remote")
	f.Set("transport", "streamable-http")
	f.Set("url", "https://mcp.example.test/sse")
	f.Set("headers", "Authorization=Bearer ${MCP_TOKEN}")
	submitForm(t, f)

	if got.GetTransport() != apiv1.MCPServerTransport_MCP_SERVER_TRANSPORT_STREAMABLE_HTTP {
		t.Fatalf("transport = %v, want STREAMABLE_HTTP", got.GetTransport())
	}
	if got.GetUrl() != "https://mcp.example.test/sse" {
		t.Fatalf("url = %q", got.GetUrl())
	}
	if got.GetHeaders()["Authorization"] != "Bearer ${MCP_TOKEN}" {
		t.Fatalf("headers = %v, want the ${SECRET_NAME} reference preserved", got.GetHeaders())
	}
}

// A CATALOG PICK PREFILLS AN OWNED CREATE — the TUI's one-click add, mirroring the GUI's
// handleCatalogAdd. The owner is stamped on the prefilled request, so a catalog pick can
// never land as a tenant-level entry.
func TestCatalogPickPrefillsAnOwnedCreate(t *testing.T) {
	catalog := []*apiv1.MCPCatalogEntry{{Slug: "github", DisplayName: "GitHub", RequiredEnv: []string{"GITHUB_TOKEN"}}}
	var got *apiv1.MCPServerCreateRequest
	f := CatalogForm(
		Owner{ProjectID: "p1"},
		func() []*apiv1.MCPCatalogEntry { return catalog },
		func(slug string) (*apiv1.MCPServerCreateRequest, error) {
			return &apiv1.MCPServerCreateRequest{
				Name:      "github",
				Transport: apiv1.MCPServerTransport_MCP_SERVER_TRANSPORT_STDIO,
				Command:   "npx",
				Args:      []string{"-y", "@mcp/github"},
			}, nil
		},
		func(r *apiv1.MCPServerCreateRequest) tea.Cmd { got = r; return nil },
	)
	f.Set("slug", "github")
	submitForm(t, f)

	if got == nil || got.GetName() != "github" || got.GetProjectId() != "p1" {
		t.Fatalf("catalog pick produced %+v, want github owned by p1", got)
	}
}

// INLINE IS THE OTHER PERSISTENCE TARGET: the worker-version form performs NO RPC — it
// writes the spec back through onSave, so a published version stays immutable.
func TestInlineFormPerformsNoRPC(t *testing.T) {
	var saved *InlineSpec
	f := InlineForm("Edit spec", &InlineSpec{ID: "github", Type: "stdio", Command: []string{"npx", "-y", "x"}},
		func(s InlineSpec) { saved = &s })
	if f == nil {
		t.Fatal("InlineForm returned nil")
	}
	if saved != nil {
		t.Fatal("opening the form performed a save — it must only write on submit")
	}
	f.Set("name", "github")
	f.Set("transport", "stdio")
	f.Set("command", "uvx y")
	submitForm(t, f)
	if saved == nil {
		t.Fatal("submit did not write the spec back")
	}
	if saved.ID != "github" || saved.Type != "stdio" || len(saved.Command) != 2 || saved.Command[0] != "uvx" {
		t.Fatalf("saved spec = %+v, want the edited inline spec", *saved)
	}
}

// PARSE/MARSHAL ROUND-TRIP, including the legacy shapes db.MCPServersFromPermissions
// accepts: a full spec, a {id, command:string} reference, and a bare id.
func TestInlineRoundTripsEveryLegacyShape(t *testing.T) {
	in := `{"tools":["read"],"mcp_servers":[
		{"id":"full","type":"stdio","command":["npx","-y","x"],"env":{"T":"${T}"}},
		{"id":"legacy","command":"npx -y y"},
		"bare-id"
	]}`
	specs := ParseInline(in)
	if len(specs) != 3 {
		t.Fatalf("parsed %d specs, want 3: %+v", len(specs), specs)
	}
	if specs[0].ID != "full" || len(specs[0].Command) != 3 {
		t.Errorf("full spec = %+v", specs[0])
	}
	if specs[1].ID != "legacy" || len(specs[1].Command) != 3 {
		t.Errorf("legacy string-command spec = %+v", specs[1])
	}
	if specs[2].ID != "bare-id" {
		t.Errorf("bare-id spec = %+v", specs[2])
	}

	// MARSHAL PRESERVES THE OTHER KEYS — the panel owns ONE key of the blob.
	out, err := MarshalInline(in, specs)
	if err != nil {
		t.Fatalf("MarshalInline: %v", err)
	}
	var back map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &back); err != nil {
		t.Fatalf("output is not a JSON object: %v", err)
	}
	if _, ok := back["tools"]; !ok {
		t.Fatalf("tools key was dropped: %s", out)
	}
	if got := ParseInline(out); len(got) != 3 {
		t.Fatalf("round-trip produced %d specs, want 3: %+v", len(got), got)
	}
}

// MERGE INTO PERMISSIONS preserves the other keys — the invariant that stops the panel's
// mcp_servers control fighting the permissions KJSON field.
func TestMergeIntoPermissionsPreservesOtherKeys(t *testing.T) {
	perms := `{"tools":["read"],"mcp_servers":[]}`
	merged, err := MergeIntoPermissions(perms, `[{"id":"github","type":"stdio","command":["npx","-y","x"]}]`)
	if err != nil {
		t.Fatalf("MergeIntoPermissions: %v", err)
	}
	var back map[string]json.RawMessage
	_ = json.Unmarshal([]byte(merged), &back)
	if _, ok := back["tools"]; !ok {
		t.Fatalf("tools key was dropped by the merge: %s", merged)
	}
	if len(ParseInline(merged)) != 1 {
		t.Fatalf("merged mcp_servers is wrong: %s", merged)
	}
}

// ParseSkillPaths is the TUI's established path-list idiom (the same rule context_files
// uses): newlines, commas, and blanks — the DELIBERATE asymmetry with the GUI's file
// browser, documented on the field.
func TestParseSkillPathsAcceptsTheContextFilesRule(t *testing.T) {
	got := ParseSkillPaths("/a/SKILL.md, /b\n\n  \n/c\n")
	want := []string{"/a/SKILL.md", "/b", "/c"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("ParseSkillPaths = %v, want %v", got, want)
	}
	if len(ParseSkillPaths("   ")) != 0 {
		t.Fatal("a blank field must parse to no paths")
	}
}

// ParseInline ACCEPTS THE BARE ARRAY as well as the wrapped object. The bare array is
// what the TUI's own mcp_servers KJSON field SHOWS and teaches (ProjectMCPDefinitionsField's
// Placeholder, and inlineJSONFromPermissions' output), so a definition typed exactly as the
// placeholder demonstrates must round-trip; accepting only the wrapped form made a
// correctly-typed definition vanish silently on save.
func TestParseInlineAcceptsTheBareArrayTheFormShows(t *testing.T) {
	bare := `[{"id":"github","type":"stdio","command":["npx","-y","x"]}]`
	got := ParseInline(bare)
	if len(got) != 1 || got[0].ID != "github" || len(got[0].Command) != 3 {
		t.Fatalf("ParseInline(bare array) = %+v, want the one github spec", got)
	}
	// The wrapped form keeps working — this ADDS a shape, it does not replace one.
	wrapped := `{"tools":["read"],"mcp_servers":` + bare + `}`
	if len(ParseInline(wrapped)) != 1 {
		t.Fatalf("ParseInline(wrapped object) = %+v, want the one github spec", ParseInline(wrapped))
	}
	// The empty array is "no definitions", not a parse failure.
	if n := len(ParseInline("[]")); n != 0 {
		t.Fatalf("ParseInline(\"[]\") = %d specs, want 0", n)
	}
}

// submitForm drives a kit2 form's own Submit (which validates and calls OnSubmit), so the
// tests exercise the real submit path rather than OnSubmit directly.
func submitForm(t *testing.T, f *kit2Form) {
	t.Helper()
	if !f.Validate() {
		t.Fatalf("form failed validation: %v", f.Errors)
	}
	if _, err := f.Submit(); err != nil {
		t.Fatalf("submit: %v", err)
	}
}

// THE SPEC FORM IS NOT WHERE A CREDENTIAL IS TYPED.
//
// The operator, on a worker version's spec form: "it still shows the field to manually type in a
// credential. We should not be doing that. Everything should be driven from the credential store."
//
// The env/headers fields carry NON-secret configuration plus the ${NAME} references the credential path
// itself writes (CredentialForm/AttachSecret) — so their labels no longer advertise the reference as
// something to type, and point at the credential verb instead. The reference stays legal INSIDE the field,
// because the picker's own write lands there; what must not be invited is the hand-typed form of it, and
// the LABEL is therefore the thing pinned here rather than a rejection of the value.
func TestSpecFormsDoNotInviteTypedCredentials(t *testing.T) {
	forms := map[string]*kit2.Form{
		"InlineForm": InlineForm("Edit spec", &InlineSpec{ID: "github", Type: "stdio"}, func(InlineSpec) {}),
		"DefineForm": DefineForm("Add server", Owner{ProjectID: "p1"}, nil,
			func(*apiv1.MCPServerCreateRequest) tea.Cmd { return nil }),
	}
	for name, f := range forms {
		seen := 0
		for _, spec := range f.Specs {
			if spec.Name != "env" && spec.Name != "headers" {
				continue
			}
			seen++
			if strings.Contains(spec.Label, "${SECRET_NAME}") {
				t.Errorf("%s: field %q still invites a TYPED reference: %q", name, spec.Name, spec.Label)
			}
			if !strings.Contains(spec.Label, "credential") {
				t.Errorf("%s: field %q does not point at the credential path: %q", name, spec.Name, spec.Label)
			}
		}
		if seen == 0 {
			t.Errorf("%s: no env/headers field found — the assertions above proved nothing", name)
		}
	}
}

// THE CREDENTIAL FORM OPENS ON WHAT THE SERVER ALREADY USES.
//
// The GUI report — "when you save an MCP server and go back into it, it doesn't list the credential that was
// already created/added. It is blank again." — described a defect the TUI shared: the entry's own env (or
// headers) carries the ${NAME} reference a previous attach wrote, and that reference IS the record of the
// attachment, so a form that opens blank is hiding it.
func TestCredentialFormsOpenOnTheServersExistingAttachment(t *testing.T) {
	const stored = "MCP_GITHUB_GITHUB_PERSONAL_ACCESS_TOKEN"
	choices := []SecretChoice{{ID: "sec-1", Name: stored}}

	// INLINE (a worker version's spec).
	spec := InlineSpec{ID: "gh", Type: "stdio",
		Env: map[string]string{"GITHUB_PERSONAL_ACCESS_TOKEN": "${" + stored + "}"}}
	f := CredentialForm("Attach", spec, choices, func(string, string, string) tea.Cmd { return nil })
	if got := f.Values["secret"]; got != stored {
		t.Errorf("the inline form opens with secret = %q, want the one already attached (%q)", got, stored)
	}
	if got := f.Values["key"]; got != "GITHUB_PERSONAL_ACCESS_TOKEN" {
		t.Errorf("the inline form opens with key = %q, want the key already carrying the reference", got)
	}

	// OWNED (a project's or conversation's row).
	row := &apiv1.MCPServer{
		Id: "s1", Name: "github", Enabled: true,
		Transport: apiv1.MCPServerTransport_MCP_SERVER_TRANSPORT_STDIO,
		Env:       map[string]string{"TOKEN": "${MCP_SHARED}"},
	}
	g := OwnedCredentialForm("Store", row, choices, func(string, string, string) tea.Cmd { return nil })
	if got := g.Values["secret"]; got != "MCP_SHARED" {
		t.Errorf("the owned form opens with secret = %q, want the one already attached", got)
	}
	if got := g.Values["key"]; got != "TOKEN" {
		t.Errorf("the owned form opens with key = %q, want TOKEN", got)
	}

	// A server with NO attachment still opens blank, so the default is not invented.
	bare := OwnedCredentialForm("Store", &apiv1.MCPServer{
		Id: "s2", Transport: apiv1.MCPServerTransport_MCP_SERVER_TRANSPORT_STDIO,
		Env: map[string]string{"LOG_LEVEL": "info"},
	}, choices, func(string, string, string) tea.Cmd { return nil })
	if got := bare.Values["secret"]; got != "" {
		t.Errorf("a server with no attachment opened with secret = %q, want blank", got)
	}
}

// The reference reader mirrors the plane's grammar, and a map is walked in SORTED order so two opens agree.
func TestAttachedReferenceReadsTheTransportAndIsDeterministic(t *testing.T) {
	env := map[string]string{"B_KEY": "${SECOND}", "A_KEY": "${FIRST}", "PLAIN": "value"}
	if k, n := attachedReference(env, nil, false); k != "A_KEY" || n != "FIRST" {
		t.Errorf("stdio attachment = (%q, %q), want the first key in sorted order", k, n)
	}
	// HEADERS for a streamable-HTTP row: the env is not what it reads.
	headers := map[string]string{"Authorization": "${REMOTE}"}
	if k, n := attachedReference(env, headers, true); k != "Authorization" || n != "REMOTE" {
		t.Errorf("http attachment = (%q, %q), want the header", k, n)
	}
	// Plain values are not references, and neither is an empty name.
	if k, _ := attachedReference(map[string]string{"A": "literal", "B": "${}"}, nil, false); k != "" {
		t.Errorf("a plain value or an empty reference was read as an attachment (key %q)", k)
	}
}
