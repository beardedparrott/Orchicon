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
		kit2.FieldSpec{Name: "env", Label: "Env (KEY=VALUE per line; non-secret values — set credentials with k)", Kind: kit2.KTextArea, Initial: env},
		// streamable HTTP connects to a remote endpoint (url + headers).
		kit2.FieldSpec{Name: "url", Label: "URL (streamable-http)", Kind: kit2.KText, Initial: url, Placeholder: "https://…"},
		kit2.FieldSpec{Name: "headers", Label: "Headers (KEY=VALUE per line; set credentials with k)", Kind: kit2.KTextArea, Initial: headers},
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

// ── credentials: the SELECTION half ──────────────────────────────────────────────────────

// SecretChoice is one SELECTABLE credential from the tenant secrets store: the NAME that goes into a
// ${...} reference, and the ID the store needs in order to REPLACE the value.
//
// THE VALUE IS NEVER FETCHED, and that is why this type has no field for one. The store is
// write-only from a client's side — ListSecrets returns names and metadata, GetSecret is never
// called — the same rule the Control screen's Secrets list follows.
type SecretChoice struct {
	ID   string
	Name string
	// Description is the store row's own description, shown beside the name so a list of
	// MCP_..._... entries stays distinguishable.
	Description string
}

// inlineSpecIsHTTP reports whether an inline spec speaks streamable HTTP. It mirrors
// mcpsettings.specFromInline's inference (type first, URL as the fallback) so the reference lands in
// the same map the spec's own transport reads from — env for stdio, headers for HTTP.
func inlineSpecIsHTTP(spec InlineSpec) bool {
	switch spec.Type {
	case "http", "streamable-http":
		return true
	case "stdio":
		return false
	}
	return spec.URL != ""
}

// CredentialKeyOptions lists the keys the spec ALREADY carries for its transport, so the common case
// — point this server's existing GITHUB_PERSONAL_ACCESS_TOKEN at a stored secret — is a PICK rather
// than a retype. An empty list is normal (a catalog pick leaves a secret key out on purpose: the
// prefill never writes a blank secret), and the field stays typable for exactly that case.
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
//
// IT WRITES INTO THE MAP THE SPEC'S TRANSPORT READS: Env for stdio, Headers for streamable HTTP — the
// same split InlineForm's own submit enforces, so the field the operator was shown is the field the
// reference lands in. An existing value at that key is REPLACED (that is the point: a plaintext
// credential becomes a reference), and the reference is passed through verbatim — it is resolved by
// the tenant secrets store at session time (mcpsettings.ResolveSecretRefs), never here.
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

// credentialFormFields is the THREE fields a credential form has at any scope: the KEY (picked from the
// keys the server declares or already carries), the SECRET (picked from the store's names), and the VALUE
// (offered only to store a new secret).
//
// THE OPERATOR'S RULE IS A PROPERTY OF THE CREDENTIAL, NOT OF THE SCOPE — "everything should be driven from
// the credential store", and "if the user wants to type in a NEW one to create one that is fine, otherwise
// it should be static" — so an owned row and an inline spec are handed the same three fields by the same
// code and cannot drift into two different answers about what a credential is.
//
// It returns the names the store holds ALONGSIDE the fields, because the submit rule is about the list that
// was just offered and belongs with the fields that offer it (credentialSubmit).
func credentialFormFields(
	initialKey string, keyOptions []kit2.Option, keyPlaceholder string, choices []SecretChoice,
) ([]kit2.FieldSpec, map[string]bool) {
	names := make([]kit2.Option, 0, len(choices))
	stored := make(map[string]bool, len(choices))
	for _, c := range choices {
		names = append(names, kit2.Option{Value: c.Name, Label: c.Name + secretChoiceSuffix(c.Description)})
		stored[c.Name] = true
	}
	return []kit2.FieldSpec{
		{Name: "key", Label: "Credential key", Kind: kit2.KPicker, Required: true,
			Initial: initialKey, Options: keyOptions, Placeholder: keyPlaceholder},
		{Name: "secret", Label: "Stored secret", Kind: kit2.KPicker, Required: true,
			Options: names, Placeholder: "pick a secret from the tenant store"},
		{Name: "value", Label: "Value (only to store a new secret)", Kind: kit2.KSecret,
			Placeholder: "leave blank to reference an existing secret"},
	}, stored
}

// credentialSubmit is the shared submit: the ONE rule the form owns, then the caller's write.
//
// The rule is about the list the form just OFFERED — a name the store does not hold cannot be referenced
// unless a value comes with it to store. Everything ELSE about a credential belongs to the plane, which can
// enforce it for every writer rather than only for this form: the key's grammar is checked when a version is
// saved (mcpsettings.ValidateInlinePermissions, which reuses the owned-row rule) and the reference resolving
// is checked when a session starts (mcpsettings.ResolveSecretRefs). Restating either here would be a second
// copy of a rule the server owns.
func credentialSubmit(
	stored map[string]bool, onSave func(key, secret, value string) tea.Cmd,
) func(map[string]string, map[string][]string) (tea.Cmd, error) {
	return func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		key := strings.TrimSpace(v["key"])
		secret := strings.TrimSpace(v["secret"])
		value := strings.TrimSpace(v["value"])
		if secret == "" {
			return nil, fmt.Errorf("pick the stored secret to reference")
		}
		if !stored[secret] && value == "" {
			return nil, fmt.Errorf("%q is not in the tenant secrets store — pick one from the list, "+
				"or give it a value to store it", secret)
		}
		return onSave(key, secret, value), nil
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
	fields, stored := credentialFormFields(initialKey, keys,
		"the name the server reads, e.g. GITHUB_PERSONAL_ACCESS_TOKEN", choices)
	f := kit2.NewForm(title, fields...)
	f.Note = "Written into this version's spec as " + spec.ID + "'s ${SECRET_NAME} reference — a " +
		"worker version has no definition row to attach a credential to. The secret is resolved from " +
		"the tenant secrets store when the worker runs."
	if len(choices) == 0 {
		f.Note += " The tenant secrets store holds nothing to pick: give the name AND a value to " +
			"store one (the name must be UPPERCASE)."
	}
	f.Focused = true
	f.Width = 64
	f.OnSubmit = credentialSubmit(stored, onSave)
	return f
}

// OwnedKeyOptions lists the keys a credential could fill for an OWNED row: what the catalog DECLARES the
// entry needs (MCPServer.required_secrets, which the plane resolves onto the row, so no catalog read is
// needed here) followed by the keys the row already carries for its transport.
//
// DECLARED FIRST, and the order is the useful one: for a catalog-added entry the declared key IS the answer
// and is therefore the default, while the keys a row already carries are usually its non-secret ones
// (PATH, LOG_LEVEL).
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

// OwnedCredentialForm points an OWNED row (a project's or a conversation's server) at a STORED credential.
//
// THE SAME THREE FIELDS AS CredentialForm, deliberately: the rule is about credentials, not scopes. It
// replaces a form that typed a Credential key and a Value straight onto the row — the shape the operator
// rejected ("it is still manually typed in key, value pair. We should not allow that option at all") — which
// stored the value against the row's id under a plane-derived name, so the value was invisible to the
// secrets store and could not be rotated from it.
//
// The write is STORE-THEN-REFERENCE, in that order and for the reason the inline path documents: the
// reference ${NAME} is resolved when a session USES the server, so a row pointing at a secret nobody stored
// fails that server's session rather than this form's submit. A name the store already holds is a ROTATION —
// the value is replaced under the same name, so every row referencing it picks the new value up at once,
// which the typed-value form could not express at all.
func OwnedCredentialForm(
	label string, row *apiv1.MCPServer, choices []SecretChoice, onApply func(key, secret, value string) tea.Cmd,
) *kit2.Form {
	keys := OwnedKeyOptions(row)
	initialKey := ""
	if len(keys) == 1 {
		initialKey = keys[0].Value
	}
	fields, stored := credentialFormFields(initialKey, keys,
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
		f.Note += " The tenant secrets store holds nothing to pick: give the name AND a value to store " +
			"one (the name must be UPPERCASE)."
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

// SkillPathsForm edits a SKILL FILES path list — the control that sits in the same
// scope surface as the MCP panel, which is why it lives here rather than beside
// any one caller.
//
// IT IS THE PATH-LIST IDIOM, DELIBERATELY (see ParseSkillPaths): the GUI selects
// skill files with a file-tree browser, the TUI has no file browser, and its
// established control for a path list is a typed field — the same treatment
// context_files already gets. Validation is the SERVER'S (contextfiles.Validate /
// ValidateWithin), never a second client-side copy, so this form only has to
// express what the operator wants.
//
// The field is PREFILLED with what the scope already holds, so changing one path
// is an edit rather than a retype, and EMPTYING it clears the list — which is the
// same write /skills clear performs.
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

// InlineSpecFromCreateRequest converts a CATALOG PREFILL (an owned-create shape) into an INLINE spec, so
// the same catalog control can seed a WORKER VERSION's list.
//
// IT EXISTS BECAUSE THE CATALOG IS A PREFILL, NOT A CREATE. The GUI's catalog Add fills the add form and
// opens it (MCPServersPanel.handleCatalogAdd → setShowForm(true)); what happens next is decided by the
// FORM's save, which writes an owned row for a project/conversation and an INLINE spec for a version
// (scope.kind === "workerVersion" → writeInline). Treating the catalog as "create a row" is what made an
// earlier TUI cut refuse the catalog at the version scope: the capability was there, the assumption was
// wrong.
//
// It is the inverse of screens/work.InlineSpecToCreateRequest, and mirrors the spec the GUI builds in its
// submit branch field for field (type from the transport, argv = command + args, env for stdio,
// url/headers for http).
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
		kit2.FieldSpec{Name: "env", Label: "Env (KEY=VALUE per line; non-secret values — set credentials with k)", Kind: kit2.KTextArea, Initial: keyValueText(spec.Env)},
		kit2.FieldSpec{Name: "url", Label: "URL (streamable-http)", Kind: kit2.KText, Initial: spec.URL},
		kit2.FieldSpec{Name: "headers", Label: "Headers (KEY=VALUE per line; set credentials with k)", Kind: kit2.KTextArea, Initial: keyValueText(spec.Headers)},
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
// It accepts the BARE ARRAY as well as the wrapped object, because the bare
// array is what the TUI's own mcp_servers field shows and teaches (see
// ProjectMCPDefinitionsField's Placeholder, and inlineJSONFromPermissions):
// accepting only the wrapped form meant a definition typed exactly as the
// placeholder demonstrates was silently dropped. MergeIntoPermissions already
// accepted both, so this closes the disagreement rather than inventing a rule.
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
