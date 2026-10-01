// Package mcpforms is the ONE TUI surface for MCP definitions, serving all
// three scopes exactly as the ONE GUI panel (MCPServersPanel.tsx) does:
// project, conversation and worker version.
//
// It exists because the old tenant-level surface — the control screen's "mcp"
// source and the launch form's select-from-tenant field — is GONE. Removing it
// without this package would have left the TUI unable to DEFINE an MCP entry at
// all, which is the exact asymmetry the epic forbids.
//
// TWO PERSISTENCE TARGETS, ONE UI (the same rule the GUI panel follows):
//
//   - OWNED ROWS (project, conversation): DefineForm builds a
//     MCPServerCreateRequest with the Owner's id set, and the caller creates it
//     through the MCP service. A definition is OWNER-SCOPED (mcp_servers.
//     project_id / conversation_id) — the owner column IS the selection.
//   - INLINE SPECS (worker version): InlineForm edits an InlineSpec and
//     MarshalInline writes the array into the version's permissions JSON. No
//     row is created, so a published version stays immutable.
//
// It depends only on kit2 + the generated api, so work, execution and chat can
// all import it without a cycle.
package mcpforms

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// Owner is the row's owner — exactly one field non-empty (the server enforces
// mcp_servers_owner_xor). It is the ONLY thing that distinguishes a
// project-owned definition from a conversation-owned one.
type Owner struct {
	ProjectID      string
	ConversationID string
}

// apply stamps the owner onto a create request. A definition with no owner
// would be a tenant-level entry, which no longer exists.
func (o Owner) apply(req *apiv1.MCPServerCreateRequest) {
	req.ProjectId = o.ProjectID
	req.ConversationId = o.ConversationID
}

// echo stamps the owner onto an update request. The owner is an ECHO on update
// (the scope is immutable after create) — a differing echo is rejected.
func (o Owner) echo(req *apiv1.MCPServerUpdateRequest) {
	req.ProjectId = o.ProjectID
	req.ConversationId = o.ConversationID
}

// mcpTransport maps the form's transport string to the wire enum, defaulting to
// stdio — the same rule the removed control form used.
func mcpTransport(s string) apiv1.MCPServerTransport {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "streamable-http", "streamable_http", "http", "remote":
		return apiv1.MCPServerTransport_MCP_SERVER_TRANSPORT_STREAMABLE_HTTP
	default:
		return apiv1.MCPServerTransport_MCP_SERVER_TRANSPORT_STDIO
	}
}

// parseKeyValue reads a "KEY=VALUE per line" field, the same shape the GUI
// form's env/headers fields use (MCPServersPanel.tsx). ${SECRET_NAME} values are
// passed through verbatim — they are resolved by the tenant secrets store at
// session time, never here.
func parseKeyValue(s string) map[string]string {
	var out map[string]string
	for _, line := range strings.Split(s, "\n") {
		idx := strings.Index(line, "=")
		if idx <= 0 {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[strings.TrimSpace(line[:idx])] = strings.TrimSpace(line[idx+1:])
	}
	return out
}

// keyValueText renders a map back into the field's text, sorted for a stable
// round-trip.
func keyValueText(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, k+"="+m[k])
	}
	return strings.Join(lines, "\n")
}

// DefineForm is the typed MCP DEFINITION form — the TUI control for
// command/args/env or url/headers, catalog-prefillable, owner-stamped.
//
// IT IS NOT THE OLD SELECT-FROM-TENANT FIELD. The removed ProjectMCPField
// listed the tenant's entries and wrote a reference selection; this form builds
// an OWNED create request, which is the capability the epic requires (AC 9).
//
// prefill, when non-nil, seeds the fields from a catalog entry — the TUI's
// one-click add, mirroring the GUI's handleCatalogAdd.
//
// onSave receives the fully-built request (owner already stamped) and returns
// the command that performs the write; the form itself never touches a client,
// so a test can assert on the request without a plane.
func DefineForm(title string, owner Owner, prefill *apiv1.MCPServerCreateRequest, onSave func(*apiv1.MCPServerCreateRequest) tea.Cmd) *kit2.Form {
	name, command, args, url := "", "", "", ""
	env, headers := "", ""
	transport := "stdio"
	enabled := "true"
	if prefill != nil {
		if prefill.GetName() != "" {
			name = prefill.GetName()
		}
		if prefill.GetCommand() != "" {
			command = prefill.GetCommand()
		}
		args = strings.Join(prefill.GetArgs(), " ")
		url = prefill.GetUrl()
		env = keyValueText(prefill.GetEnv())
		headers = keyValueText(prefill.GetHeaders())
		switch prefill.GetTransport() {
		case apiv1.MCPServerTransport_MCP_SERVER_TRANSPORT_STREAMABLE_HTTP:
			transport = "streamable-http"
		}
		if !prefill.GetEnabled() {
			enabled = "false"
		}
	}
	f := kit2.NewForm(title,
		kit2.FieldSpec{Name: "name", Label: "Name", Kind: kit2.KText, Required: true, Initial: name, Placeholder: "github-mcp"},
		kit2.FieldSpec{Name: "transport", Label: "Transport", Kind: kit2.KSelect, Initial: transport, Options: []kit2.Option{
			{Value: "stdio", Label: "stdio"}, {Value: "streamable-http", Label: "streamable-http"},
		}},
		// stdio runs a subprocess (command + args + env).
		kit2.FieldSpec{Name: "command", Label: "Command (stdio)", Kind: kit2.KText, Initial: command, Placeholder: "npx"},
		kit2.FieldSpec{Name: "args", Label: "Args (space separated)", Kind: kit2.KText, Initial: args},
		kit2.FieldSpec{Name: "env", Label: "Env (KEY=VALUE per line; ${SECRET_NAME} allowed)", Kind: kit2.KTextArea, Initial: env},
		// streamable HTTP connects to a remote endpoint (url + headers).
		kit2.FieldSpec{Name: "url", Label: "URL (streamable-http)", Kind: kit2.KText, Initial: url, Placeholder: "https://…"},
		kit2.FieldSpec{Name: "headers", Label: "Headers (KEY=VALUE per line; ${SECRET_NAME} allowed)", Kind: kit2.KTextArea, Initial: headers},
		kit2.FieldSpec{Name: "enabled", Label: "Enabled", Kind: kit2.KCheckbox, Initial: enabled},
	)
	f.Focused = true
	f.Width = 64
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		name := strings.TrimSpace(v["name"])
		if name == "" {
			return nil, fmt.Errorf("a server name is required")
		}
		en := v["enabled"] == "true"
		req := &apiv1.MCPServerCreateRequest{
			Name:      name,
			Transport: mcpTransport(v["transport"]),
			Command:   strings.TrimSpace(v["command"]),
			Args:      strings.Fields(v["args"]),
			Env:       parseKeyValue(v["env"]),
			Url:       strings.TrimSpace(v["url"]),
			Headers:   parseKeyValue(v["headers"]),
			Enabled:   en,
		}
		owner.apply(req)
		return onSave(req), nil
	}
	return f
}

// CatalogForm lists the curated registry and, on pick, opens DefineForm
// prefilled from the catalog entry — the TUI's one-click add. The prefill RPC is
// the caller's (it already has a client); this form only carries the choice.
//
// list returns the catalog entries, prefetch returns one entry's prefill. Both
// are injected so the form is testable without a plane, and so the form does not
// re-implement the catalog's install-mechanism logic.
func CatalogForm(
	owner Owner,
	list func() []*apiv1.MCPCatalogEntry,
	prefill func(slug string) (*apiv1.MCPServerCreateRequest, error),
	onSave func(*apiv1.MCPServerCreateRequest) tea.Cmd,
) *kit2.Form {
	entries := list()
	opts := make([]kit2.Option, 0, len(entries))
	for _, e := range entries {
		label := e.GetDisplayName()
		if e.GetSlug() == "" {
			label = e.GetSlug()
		}
		if len(e.GetRequiredEnv()) > 0 {
			label += " (secrets: " + strings.Join(e.GetRequiredEnv(), ", ") + ")"
		}
		opts = append(opts, kit2.Option{Value: e.GetSlug(), Label: label})
	}
	f := kit2.NewForm("Add MCP server from the catalog",
		kit2.FieldSpec{Name: "slug", Label: "Catalog entry", Kind: kit2.KSelect, Required: true, Options: opts},
	)
	f.Focused = true
	f.Width = 64
	// The chosen slug PREFILLS an owned create and saves it — a catalog pick is
	// the TUI's one-click add, mirroring the GUI's handleCatalogAdd (which also
	// prefills then creates). The prefill RPC is injected, so this form does not
	// re-implement the catalog's install-mechanism logic.
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		slug := strings.TrimSpace(v["slug"])
		if slug == "" {
			return nil, fmt.Errorf("pick a catalog entry")
		}
		p, err := prefill(slug)
		if err != nil {
			return nil, err
		}
		if p == nil {
			p = &apiv1.MCPServerCreateRequest{}
		}
		owner.apply(p)
		return onSave(p), nil
	}
	return f
}

// InstallForm is the explicit auto-install control for an owned entry. An
// inline worker spec has none — it has no row and no install status.
func InstallForm(label string, onInstall func() tea.Cmd) *kit2.Form {
	f := kit2.NewForm("Install "+label,
		kit2.FieldSpec{Name: "confirm", Label: "Run auto-install (explicit)? (yes/no)", Kind: kit2.KText, Initial: "yes"},
	)
	f.Focused = true
	f.Width = 64
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		if strings.ToLower(strings.TrimSpace(v["confirm"])) != "yes" {
			return nil, nil
		}
		return onInstall(), nil
	}
	return f
}

// SecretForm writes a credential for an owned entry's required secret. The value
// field is a KSecret — masked in the view, written once, never read back. The
// secrets store is tenant-scoped (RLS needs a tenant); that is a credential
// store, not an MCP scope.
func SecretForm(label, key string, onSet func(key, value string) tea.Cmd) *kit2.Form {
	f := kit2.NewForm("Store MCP credential: "+label,
		kit2.FieldSpec{Name: "name", Label: "Credential key", Kind: kit2.KText, Required: true, Initial: key, Placeholder: "GITHUB_TOKEN"},
		kit2.FieldSpec{Name: "value", Label: "Value", Kind: kit2.KSecret, Required: true},
	)
	f.Focused = true
	f.Width = 64
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		return onSet(v["name"], v["value"]), nil
	}
	return f
}

// InlineSpec mirrors db.InlineMCPServer (internal/db/mcp_servers.go) — the ONE
// shape the server already parses out of a worker version's
// permissions.mcp_servers. The worker-version placement writes THIS array rather
// than an owned row, so a published version stays immutable.
type InlineSpec struct {
	ID      string            `json:"id"`
	Type    string            `json:"type,omitempty"` // "stdio" | "http"
	Command []string          `json:"command,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Enabled *bool             `json:"enabled,omitempty"`
	OnError string            `json:"onError,omitempty"`
}

// InlineForm edits one inline spec and writes it back through onSave — no RPC,
// no row. command/args/env for stdio, url/headers for http, same as DefineForm;
// the difference is purely the persistence target (AC 3: one UI, two targets).
func InlineForm(title string, src *InlineSpec, onSave func(InlineSpec)) *kit2.Form {
	spec := InlineSpec{}
	if src != nil {
		spec = *src
	}
	transport := "stdio"
	if spec.Type == "http" {
		transport = "streamable-http"
	}
	enabled := "true"
	if spec.Enabled != nil && !*spec.Enabled {
		enabled = "false"
	}
	f := kit2.NewForm(title,
		kit2.FieldSpec{Name: "name", Label: "Name", Kind: kit2.KText, Required: true, Initial: spec.ID, Placeholder: "github-mcp"},
		kit2.FieldSpec{Name: "transport", Label: "Transport", Kind: kit2.KSelect, Initial: transport, Options: []kit2.Option{
			{Value: "stdio", Label: "stdio"}, {Value: "streamable-http", Label: "streamable-http"},
		}},
		kit2.FieldSpec{Name: "command", Label: "Command (stdio)", Kind: kit2.KText, Initial: strings.Join(spec.Command, " ")},
		kit2.FieldSpec{Name: "env", Label: "Env (KEY=VALUE per line; ${SECRET_NAME} allowed)", Kind: kit2.KTextArea, Initial: keyValueText(spec.Env)},
		kit2.FieldSpec{Name: "url", Label: "URL (streamable-http)", Kind: kit2.KText, Initial: spec.URL},
		kit2.FieldSpec{Name: "headers", Label: "Headers (KEY=VALUE per line)", Kind: kit2.KTextArea, Initial: keyValueText(spec.Headers)},
		kit2.FieldSpec{Name: "enabled", Label: "Enabled", Kind: kit2.KCheckbox, Initial: enabled},
	)
	f.Focused = true
	f.Width = 64
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		id := strings.TrimSpace(v["name"])
		if id == "" {
			return nil, fmt.Errorf("a spec id is required")
		}
		en := v["enabled"] == "true"
		next := InlineSpec{ID: id, Enabled: &en, OnError: spec.OnError}
		if mcpTransport(v["transport"]) == apiv1.MCPServerTransport_MCP_SERVER_TRANSPORT_STREAMABLE_HTTP {
			next.Type = "http"
			next.URL = strings.TrimSpace(v["url"])
			next.Headers = parseKeyValue(v["headers"])
		} else {
			next.Type = "stdio"
			next.Command = strings.Fields(v["command"])
			next.Env = parseKeyValue(v["env"])
		}
		onSave(next)
		return nil, nil
	}
	return f
}

// ParseInline decodes a permissions-sourced inline array. It accepts the THREE
// shapes db.MCPServersFromPermissions accepts, so a legacy version round-trips:
//
//  1. full spec   {"id","type","command":["npx","-y","x"],…}
//  2. legacy ref  {"id":"01H…","command":"npx -y x"}  (command is a string)
//  3. bare id     "01H…"
//
// An empty or malformed input yields nil (a shape error must not block a form
// from opening — the same degradation the server does).
func ParseInline(permissionsJSON string) []InlineSpec {
	if strings.TrimSpace(permissionsJSON) == "" {
		return nil
	}
	var p struct {
		MCPServers []json.RawMessage `json:"mcp_servers"`
	}
	if err := json.Unmarshal([]byte(permissionsJSON), &p); err != nil {
		return nil
	}
	var out []InlineSpec
	for _, raw := range p.MCPServers {
		s := strings.TrimSpace(string(raw))
		switch {
		case s == "" || s == "null":
			continue
		case strings.HasPrefix(s, "{"):
			var o struct {
				InlineSpec
				Command json.RawMessage `json:"command,omitempty"`
			}
			if err := json.Unmarshal(raw, &o); err != nil || o.ID == "" {
				continue
			}
			spec := o.InlineSpec
			spec.Command = decodeInlineCommand(o.Command)
			out = append(out, spec)
		default:
			id := strings.Trim(strings.TrimSpace(s), `"`)
			if id != "" {
				out = append(out, InlineSpec{ID: id})
			}
		}
	}
	return out
}

// decodeInlineCommand decodes the "command" member, which is EITHER the argv
// array OR the legacy single string (split on fields).
func decodeInlineCommand(raw json.RawMessage) []string {
	t := strings.TrimSpace(string(raw))
	if t == "" || t == "null" {
		return nil
	}
	if strings.HasPrefix(t, "[") {
		var argv []string
		if err := json.Unmarshal(raw, &argv); err == nil {
			return argv
		}
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.Fields(s)
	}
	return nil
}

// MarshalInline encodes the specs back into the permissions JSON, replacing the
// mcp_servers key and PRESERVING every other key (tools, model_providers, …) —
// the panel owns one key of the blob, never the whole blob.
func MarshalInline(permissionsJSON string, specs []InlineSpec) (string, error) {
	p := map[string]json.RawMessage{}
	if strings.TrimSpace(permissionsJSON) != "" {
		if err := json.Unmarshal([]byte(permissionsJSON), &p); err != nil {
			return "", fmt.Errorf("permissions is not a JSON object: %w", err)
		}
	}
	if specs == nil {
		specs = []InlineSpec{}
	}
	raw, err := json.Marshal(specs)
	if err != nil {
		return "", err
	}
	p["mcp_servers"] = raw
	out, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ParseSkillPaths reads the skill_files path-list field: one path per line, with
// commas also accepted (a single-line paste of "a, b" is the natural mistake) and
// blanks dropped. It is the SAME rule context_files uses (screens/work.
// ParseContextFiles); validation is the SERVER'S (contextfiles.Validate /
// ValidateWithin), never a second client-side copy. The GUI selects skill files
// with a file-tree browser; the TUI has no file browser, so it uses this typed
// path-list idiom — the deliberate asymmetry documented on
// screens/work/projects.go's skillFilesLabel.
func ParseSkillPaths(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		for _, part := range strings.Split(line, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// MergeIntoPermissions is MarshalInline under the name the worker-version
// setters call: the panel OWNS the mcp_servers key of the permissions blob, so
// the setters merge the form's inline array into whatever the permissions KJSON
// field carries rather than replacing the blob. Documented here because the
// ownership is the invariant that stops the two controls fighting.
func MergeIntoPermissions(permissionsJSON, mcpJSON string) (string, error) {
	var specs []InlineSpec
	if strings.TrimSpace(mcpJSON) != "" && strings.TrimSpace(mcpJSON) != "[]" {
		// mcpJSON is either a bare array or a full permissions object.
		if strings.HasPrefix(strings.TrimSpace(mcpJSON), "[") {
			if err := json.Unmarshal([]byte(mcpJSON), &specs); err != nil {
				return "", err
			}
		} else {
			specs = ParseInline(mcpJSON)
		}
	}
	return MarshalInline(permissionsJSON, specs)
}
