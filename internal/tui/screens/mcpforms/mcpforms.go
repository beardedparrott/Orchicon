// Package mcpforms is the ONE TUI surface for MCP definitions, serving all three scopes exactly as
// the ONE GUI panel (MCPServersPanel.tsx) does: project, conversation and worker version.
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

// It is the ONLY thing that distinguishes a project-owned definition from a conversation-owned one.
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

// parseKeyValue reads a "KEY=VALUE per line" field, the same shape the GUI form's env/headers
// fields use (MCPServersPanel.tsx).
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

// DefineForm is the typed MCP DEFINITION form — the TUI control for command/args/env or
// url/headers, catalog-prefillable, owner-stamped.
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
		kit2.FieldSpec{Name: "env", Label: "Enter a KEY=VALUE pair here (non-secret stored) or use k to create/store secret credentials", Kind: kit2.KTextArea, Initial: env},
		// streamable HTTP connects to a remote endpoint (url + headers).
		kit2.FieldSpec{Name: "url", Label: "URL (streamable-http)", Kind: kit2.KText, Initial: url, Placeholder: "https://…"},
		kit2.FieldSpec{Name: "headers", Label: "Enter a KEY=VALUE pair here (non-secret stored) or use k to create/store secret credentials", Kind: kit2.KTextArea, Initial: headers},
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

// CatalogForm lists the curated registry and, on pick, opens DefineForm prefilled from the catalog
// entry — the TUI's one-click add.
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
	// The chosen slug PREFILLS an owned create and saves it — a catalog pick is the TUI's one-click
	// add, mirroring the GUI's handleCatalogAdd (which also prefills then creates).
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

// ── credentials: the SELECTION half ──────────────────────────────────────────────────────

// THE VALUE IS NEVER FETCHED, and that is why this type has no field for one.
type SecretChoice struct {
	ID   string
	Name string
	// Description is the store row's own description, shown beside the name so a list of
	// MCP_..._... entries stays distinguishable.
	Description string
}

// inlineSpecIsHTTP reports whether an inline spec speaks streamable HTTP.
func inlineSpecIsHTTP(spec InlineSpec) bool {
	switch spec.Type {
	case "http", "streamable-http":
		return true
	case "stdio":
		return false
	}
	return spec.URL != ""
}

// CredentialKeyOptions lists the keys the spec ALREADY carries for its transport, so the common
// case — point this server's existing GITHUB_PERSONAL_ACCESS_TOKEN at a stored secret.
func CredentialKeyOptions(spec InlineSpec) []kit2.Option {
	m := spec.Env
	if inlineSpecIsHTTP(spec) {
		m = spec.Headers
	}
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]kit2.Option, 0, len(keys))
	for _, k := range keys {
		label := k
		if v := strings.TrimSpace(m[k]); v != "" {
			label += " (currently " + v + ")"
		}
		out = append(out, kit2.Option{Value: k, Label: label})
	}
	return out
}

// AttachSecret points ONE key of an inline spec at a stored tenant secret, as ${NAME}.
func AttachSecret(spec *InlineSpec, key, secretName string) {
	key, secretName = strings.TrimSpace(key), strings.TrimSpace(secretName)
	if key == "" || secretName == "" {
		return
	}
	ref := "${" + secretName + "}"
	if inlineSpecIsHTTP(*spec) {
		if spec.Headers == nil {
			spec.Headers = map[string]string{}
		}
		spec.Headers[key] = ref
		return
	}
	if spec.Env == nil {
		spec.Env = map[string]string{}
	}
	spec.Env[key] = ref
}

// CredentialForm attaches a STORED credential to one INLINE spec (a worker version's).
//
// WHY IT EXISTS. A worker version has no row, so `k` on one of its specs could only say "put the
// ${SECRET_NAME} in the spec's env/headers instead" — the operator: "the credentials should be able
// to be selected rather than typed just like it does for projects and conversations. I understand
// that it is inline but that doesn't mean that info can't be built on top and passed to the config."
// It can: the reference is built here and written into the spec's own config, which is the half that
// was missing.
//
// THE SELECTION IS THE POINT. `secret` is a PICKER over the tenant secrets store's names, which is
// what "selected rather than typed" means and what makes the reference RESOLVABLE: a name that is not
// stored fails that server's session at resolve time (mcpsettings.ResolveSecretRefs), so a free-typed
// name would be a promise the store cannot keep. It stays typable for one case only — a name given
// together with a value, which the caller STORES first (see onSave's contract).
//
// onSave receives (key, secretName, value): value is "" when an existing secret is simply referenced,
// and non-empty when the operator asked for that name to be created (or replaced) in the store. THE
// CALLER OWNS BOTH WRITES and their order — the store must land before the version does, or the
// version would reference a secret that is not there.
//
// THE KEY'S GRAMMAR IS THE PLANE'S RULE, not a copy kept here: an inline spec's env/header keys (and
// every ${SECRET_NAME} it references) are validated at version save
// (internal/mcpsettings/validate_inline.go), which is also the gate for the clients that do not come
// through this form at all. The one rule the form DOES own is about the list it just offered: a name
// that is not in the tenant store cannot be referenced, so it needs a value to create it.
// ── the credential surface: ONE definition, every scope ──────────────────────────────────

// CustomSecretChoice is the stored-secret picker's entry that means "store a NEW secret, named and
// valued in the fields below".
const CustomSecretChoice = "__custom__"

// credentialFormFields is the ONE definition of the fields a credential form has at any scope: the
// KEY (picked from the keys the server declares or already carries).
func credentialFormFields(
	initialKey, initialSecret string, keyOptions []kit2.Option, keyPlaceholder string, choices []SecretChoice,
) ([]kit2.FieldSpec, map[string]bool) {
	names := make([]kit2.Option, 0, len(choices)+1)
	stored := make(map[string]bool, len(choices))
	for _, c := range choices {
		names = append(names, kit2.Option{Value: c.Name, Label: c.Name + secretChoiceSuffix(c.Description)})
		stored[c.Name] = true
	}
	names = append(names, kit2.Option{Value: CustomSecretChoice, Label: "Custom — store a new secret (name it below)"})
	custom := func(v map[string]string) bool { return v["secret"] == CustomSecretChoice }
	return []kit2.FieldSpec{
		{Name: "key", Label: "Credential key", Kind: kit2.KPicker, Required: true,
			Initial: initialKey, Options: keyOptions, Placeholder: keyPlaceholder},
		// SEEDED WITH WHAT THE SERVER ALREADY USES, when it already references one.
		{Name: "secret", Label: "Stored secret", Kind: kit2.KPicker, Required: true,
			Initial: initialSecret, Options: names, Placeholder: "pick a stored secret, or choose Custom"},
		// Hidden while a STORED secret is picked, so the two halves cannot be told at once — which is
		// exactly the confusion this replaces.
		{Name: "newname", Label: "New secret name", Kind: kit2.KText, Required: true,
			Visible: custom, Placeholder: "e.g. MCP_SLACK_BOT_TOKEN (UPPERCASE)"},
		{Name: "value", Label: "New secret value", Kind: kit2.KSecret, Required: true,
			Visible: custom, Placeholder: "write-only — kept in the tenant secrets store"},
	}, stored
}

// credentialSubmit is the shared submit: the rules the form owns, then the caller's write.
func credentialSubmit(
	stored map[string]bool, onSave func(key, secret, value string) tea.Cmd,
) func(map[string]string, map[string][]string) (tea.Cmd, error) {
	return func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		key := strings.TrimSpace(v["key"])
		secret := strings.TrimSpace(v["secret"])
		if secret == CustomSecretChoice {
			name := strings.TrimSpace(v["newname"])
			value := strings.TrimSpace(v["value"])
			if name == "" {
				return nil, fmt.Errorf("name the new secret")
			}
			if value == "" {
				return nil, fmt.Errorf("give %s a value to store", name)
			}
			return onSave(key, name, value), nil
		}
		switch {
		case secret == "":
			return nil, fmt.Errorf("pick the stored secret to reference, or choose Custom to store a new one")
		case !stored[secret]:
			return nil, fmt.Errorf("%q is not in the tenant secrets store — pick one from the list, or "+
				"choose Custom to store it", secret)
		}
		// A STORED SECRET IS REFERENCED WITH NO VALUE, and a value left over from before the pick is
		// REFUSED rather than ignored.
		if strings.TrimSpace(v["value"]) != "" {
			return nil, fmt.Errorf("%s is already stored — its value is not re-sent. Choose Custom to store "+
				"a new or rotated value", secret)
		}
		return onSave(key, secret, ""), nil
	}
}

// CredentialForm points one of a WORKER VERSION's inline specs at a stored credential.
func CredentialForm(title string, spec InlineSpec, choices []SecretChoice, onSave func(key, secret, value string) tea.Cmd) *kit2.Form {
	// The spec's own key is PRESELECTED when there is exactly one, so the common case is two keystrokes
	// (open, pick the secret) rather than a decision about a name that only has one answer.
	keys := CredentialKeyOptions(spec)
	initialKey := ""
	if len(keys) == 1 {
		initialKey = keys[0].Value
	}
	attachedKey, attachedSecret := attachedReference(spec.Env, spec.Headers, inlineSpecIsHTTP(spec))
	if attachedKey != "" {
		initialKey = attachedKey
	}
	fields, stored := credentialFormFields(initialKey, attachedSecret, keys,
		"the name the server reads, e.g. GITHUB_PERSONAL_ACCESS_TOKEN", choices)
	f := kit2.NewForm(title, fields...)
	f.Note = "Written into this version's spec as " + spec.ID + "'s ${SECRET_NAME} reference — a " +
		"worker version has no definition row to attach a credential to. The secret is resolved from " +
		"the tenant secrets store when the worker runs."
	if len(choices) == 0 {
		f.Note += " The tenant secrets store holds nothing to pick yet: choose Custom to name and " +
			"store one (the name must be UPPERCASE)."
	}
	f.Focused = true
	f.Width = 64
	f.OnSubmit = credentialSubmit(stored, onSave)
	return f
}

// secretRefName reads the FIRST ${NAME} reference a value carries, mirroring the plane's own grammar
// (mcpsettings.scanSecretRefs). It reads the reference WHEREVER it sits — "Bearer ${TOKEN}" is one —
// because the plane expands it there too: a whole-value-only mirror showed an entry whose header was
// "Bearer ${TOKEN}" as having no credential attached, which is precisely the configuration the plane
// now resolves.
func secretRefName(v string) string {
	if name := secretRefNamesIn(v); len(name) > 0 {
		return name[0]
	}
	return ""
}

// secretRefNamesIn lists every ${NAME} reference in v, in order, mirroring the plane's
// mcpsettings.secretRefNames. The name grammar is the tenant secret-name rule, so a brace expression
// in ordinary text ("${5}", "{}") is not a reference here any more than it is plane-side.
func secretRefNamesIn(v string) []string {
	var out []string
	for i := 0; i < len(v); i++ {
		if v[i] != '$' || i+2 >= len(v) || v[i+1] != '{' {
			continue
		}
		j := i + 2
		if !secretRefNameStart(v[j]) {
			continue
		}
		j++
		for j < len(v) && secretRefNameByte(v[j]) {
			j++
		}
		if j >= len(v) || v[j] != '}' {
			continue
		}
		out = append(out, v[i+2:j])
		i = j
	}
	return out
}

func secretRefNameStart(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func secretRefNameByte(b byte) bool {
	return secretRefNameStart(b) || (b >= '0' && b <= '9')
}

// attachedReference reports which key a server ALREADY points at a stored secret, and at which
// secret.
func attachedReference(env, headers map[string]string, isHTTP bool) (key, secretName string) {
	m := env
	if isHTTP {
		m = headers
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if name := secretRefName(m[k]); name != "" {
			return k, name
		}
	}
	return "", ""
}

// isHTTPRow reports whether an owned row reads HEADERS rather than env.
func isHTTPRow(row *apiv1.MCPServer) bool {
	return row.GetTransport() == apiv1.MCPServerTransport_MCP_SERVER_TRANSPORT_STREAMABLE_HTTP
}

// OwnedKeyOptions lists the keys a credential could fill for an OWNED row: what the catalog
// DECLARES the entry needs (MCPServer.required_secrets, which the plane resolves onto the row.
func OwnedKeyOptions(row *apiv1.MCPServer) []kit2.Option {
	existing := row.GetEnv()
	if row.GetTransport() == apiv1.MCPServerTransport_MCP_SERVER_TRANSPORT_STREAMABLE_HTTP {
		existing = row.GetHeaders()
	}
	declared := row.GetRequiredSecrets()
	seen := make(map[string]bool, len(existing)+len(declared))
	out := make([]kit2.Option, 0, len(existing)+len(declared))
	add := func(k string) {
		k = strings.TrimSpace(k)
		if k == "" || seen[k] {
			return
		}
		seen[k] = true
		out = append(out, kit2.Option{Value: k, Label: k})
	}
	for _, k := range declared {
		add(k)
	}
	// Sorted, so the picker offers the same order on two reads of the same row.
	keys := make([]string, 0, len(existing))
	for k := range existing {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		add(k)
	}
	return out
}

// OwnedCredentialForm points an OWNED row (a project's or a conversation's server) at a STORED
// credential.
func OwnedCredentialForm(
	label string, row *apiv1.MCPServer, choices []SecretChoice, onApply func(key, secret, value string) tea.Cmd,
) *kit2.Form {
	keys := OwnedKeyOptions(row)
	initialKey := ""
	if len(keys) == 1 {
		initialKey = keys[0].Value
	}
	attachedKey, attachedSecret := attachedReference(row.GetEnv(), row.GetHeaders(), isHTTPRow(row))
	if attachedKey != "" {
		initialKey = attachedKey
	}
	fields, stored := credentialFormFields(initialKey, attachedSecret, keys,
		"the name this server reads, e.g. GITHUB_PERSONAL_ACCESS_TOKEN", choices)
	f := kit2.NewForm("Store MCP credential: "+label, fields...)
	transport := "env"
	if row.GetTransport() == apiv1.MCPServerTransport_MCP_SERVER_TRANSPORT_STREAMABLE_HTTP {
		transport = "headers"
	}
	f.Note = "Written into this row's " + transport + " as a ${SECRET_NAME} reference. The value itself is " +
		"never stored on the server: it lives in the tenant secrets store and is resolved when a session " +
		"uses the server."
	if len(choices) == 0 {
		f.Note += " The tenant secrets store holds nothing to pick yet: choose Custom to name and " +
			"store one (the name must be UPPERCASE)."
	}
	f.Focused = true
	f.Width = 64
	f.OnSubmit = credentialSubmit(stored, onApply)
	return f
}

// secretChoiceSuffix renders a stored secret's description beside its name, when it has one.
func secretChoiceSuffix(desc string) string {
	desc = strings.TrimSpace(desc)
	if desc == "" {
		return ""
	}
	return " — " + desc
}

// SkillPathsForm edits a SKILL FILES path list — the control that sits in the same scope surface as
// the MCP panel, which is why it lives here rather than beside any one caller.
func SkillPathsForm(current string, onSave func(paths []string) tea.Cmd) *kit2.Form {
	f := kit2.NewForm("Skill files for this conversation",
		kit2.FieldSpec{
			Name: "paths", Label: "Paths (one per line, or comma-separated)",
			Kind: kit2.KTextArea, Initial: current,
			Placeholder: "skills/review.md  ·  docs/ (a directory is read in full)",
		},
	)
	f.Note = "Real on-disk paths in the conversation's project directory. The union of these and the " +
		"project's skill files is rendered into the prompt. Clearing the field removes them all."
	f.Focused = true
	f.Width = 64
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		// An emptied field is a CLEAR, not a no-op: the operator who deleted every
		// line has said what they mean, and ParseSkillPaths already drops blanks.
		return onSave(ParseSkillPaths(v["paths"])), nil
	}
	return f
}

// InlineSpecFromCreateRequest converts a CATALOG PREFILL (an owned-create shape) into an INLINE
// spec, so the same catalog control can seed a WORKER VERSION's list.
func InlineSpecFromCreateRequest(req *apiv1.MCPServerCreateRequest) InlineSpec {
	enabled := req.GetEnabled()
	out := InlineSpec{ID: req.GetName(), Enabled: &enabled}
	if req.GetTransport() == apiv1.MCPServerTransport_MCP_SERVER_TRANSPORT_STREAMABLE_HTTP {
		out.Type, out.URL, out.Headers = "http", req.GetUrl(), req.GetHeaders()
		return out
	}
	out.Type = "stdio"
	if cmd := strings.TrimSpace(req.GetCommand()); cmd != "" {
		// argv is command FIRST, then the args — the shape db.MCPServerFromPermissions reads back.
		out.Command = append([]string{cmd}, req.GetArgs()...)
	}
	out.Env = req.GetEnv()
	return out
}

// InlineSpec mirrors db.InlineMCPServer (internal/db/mcp_servers.go) — the ONE shape the server
// already parses out of a worker version's permissions.mcp_servers.
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

// InlineForm edits one inline spec and writes it back through onSave — no RPC, no row.
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
		kit2.FieldSpec{Name: "env", Label: "Enter a KEY=VALUE pair here (non-secret stored) or use k to create/store secret credentials", Kind: kit2.KTextArea, Initial: keyValueText(spec.Env)},
		kit2.FieldSpec{Name: "url", Label: "URL (streamable-http)", Kind: kit2.KText, Initial: spec.URL},
		kit2.FieldSpec{Name: "headers", Label: "Enter a KEY=VALUE pair here (non-secret stored) or use k to create/store secret credentials", Kind: kit2.KTextArea, Initial: keyValueText(spec.Headers)},
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

// ParseInline decodes a permissions-sourced inline array.
func ParseInline(permissionsJSON string) []InlineSpec {
	t := strings.TrimSpace(permissionsJSON)
	if t == "" {
		return nil
	}
	var raw []json.RawMessage
	if strings.HasPrefix(t, "[") {
		if err := json.Unmarshal([]byte(t), &raw); err != nil {
			return nil
		}
	} else {
		var p struct {
			MCPServers []json.RawMessage `json:"mcp_servers"`
		}
		if err := json.Unmarshal([]byte(t), &p); err != nil {
			return nil
		}
		raw = p.MCPServers
	}
	var out []InlineSpec
	for _, raw := range raw {
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

// MarshalInline encodes the specs back into the permissions JSON, replacing the mcp_servers key and
// PRESERVING every other key (tools, model_providers, …) — the panel owns one key of the blob.
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

// ParseSkillPaths reads the skill_files path-list field: one path per line, with commas also
// accepted (a single-line paste of "a, b" is the natural mistake) and blanks dropped.
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

// MergeIntoPermissions is MarshalInline under the name the worker-version setters call: the panel
// OWNS the mcp_servers key of the permissions blob.
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
