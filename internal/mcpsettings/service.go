// Package mcpsettings implements the tenant-facing MCP server management
// surface behind Settings → Adapters → MCP: CRUD over OWNER-SCOPED MCP
// definitions (stdio + streamable HTTP), the curated registry catalog with
// install specs, explicit-only auto-install (runtime detection + dry-run
// for CI), and write-only credentials via the tenant secrets store
// (provider-token pattern, ADR-0006 D5).
//
// A definition belongs to EXACTLY ONE owner — a project OR an Ask
// conversation (mcp_servers.project_id XOR mcp_servers.conversation_id).
// There is no tenant-wide tier and no selection join table: the owner
// column IS the selection, so a definition can never be orphaned and the
// deletion guard that existed only because a definition could be shared is
// gone with it.
//
// Resolution is ONE union, addressed by SCOPE — see Resolver and
// mcpclient.ScopeResolver. The old three-place precedence chain (worker →
// project → tenant default → none) is replaced by
// project-owned ∪ scope-owned, deduped, order-stable.
package mcpsettings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/beardedparrott/orchicon/internal/audit"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/secretcrypto"
	"github.com/beardedparrott/orchicon/internal/secrets"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Transport kinds.
const (
	TransportStdio      = "stdio"
	TransportStreamable = "streamable-http"
)

// Install statuses (mirror MCPInstallStatus enum values).
const (
	InstallUnknown      = "unknown"
	InstallNotInstalled = "not_installed"
	InstallInstalling   = "installing"
	InstallInstalled    = "installed"
	InstallFailed       = "failed"
)

// maxNameLen bounds entry names (mirrors the providers display-name bound).
const maxNameLen = 200

// maxCommandLen bounds stdio commands.
const maxCommandLen = 1024

// maxArgLen / maxArgs bounds the argv array.
const maxArgLen = 256
const maxArgs = 64

// maxURLValueLen bounds URLs.
const maxURLValueLen = 2048

// envKeyRE is the env/header key grammar.
const envKeyPattern = `^[A-Za-z_][A-Za-z0-9_]*$`

var envKeyRE = regexp.MustCompile(envKeyPattern)

// errInvalidArgument marks validation failures (handler maps to
// connect.CodeInvalidArgument — no error-string sniffing).
var errInvalidArgument = errors.New("invalid argument")

func invalidf(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errInvalidArgument, fmt.Sprintf(format, a...))
}

// Entry is one MCP server row as the service exposes it. Plaintext
// credentials never appear here.
type Entry struct {
	ID   string
	Name string
	// ProjectID / ConversationID are the OWNER — exactly one is non-empty.
	ProjectID       string
	ConversationID  string
	Transport       string
	Command         string
	Args            []string
	Env             map[string]string // values may be ${SECRET_NAME}
	URL             string
	Headers         map[string]string
	Enabled         bool
	CatalogSlug     string
	InstallStatus   string
	InstallResult   InstallResult
	RequiredSecrets []string
	HasSecretStored bool
	CreatedAt       string // RFC3339
	UpdatedAt       string // RFC3339
}

// InstallResult is the recorded auto-install outcome.
type InstallResult struct {
	Runtime     string `json:"runtime"`
	Command     string `json:"command"`
	OK          bool   `json:"ok"`
	Error       string `json:"error,omitempty"`
	InstalledAt string `json:"installed_at,omitempty"`
}

// Service is the MCP settings core: storage over the OWNER-SCOPED
// mcp_servers table, catalog and auto-install logic, and write-only secret
// wiring.
type Service struct {
	pool *db.Pool
	kek  []byte
	log  *slog.Logger
}

// New constructs the service. kek may be nil/short — the secrets store is
// then disabled (fail-closed on credential writes), mirroring providers.
func New(pool *db.Pool, kek []byte, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{pool: pool, kek: kek, log: log}
}

func (s *Service) requireKEK() error {
	if len(s.kek) != 32 {
		return fmt.Errorf("secrets store unavailable: KEK not configured")
	}
	return nil
}

func (s *Service) begin(ctx context.Context, tenantID string) (*db.TenantTx, error) {
	return s.pool.BeginTenantTx(ctx, tenantID)
}

func entryFromRow(r db.MCPServerRow) Entry {
	e := Entry{
		ID:             r.ID,
		Name:           r.Name,
		ProjectID:      r.ProjectID,
		ConversationID: r.ConversationID,
		Transport:      r.Transport,
		Command:        r.Command,
		Args:           r.Args,
		Env:            r.Env,
		URL:            r.URL,
		Headers:        r.Headers,
		Enabled:        r.Enabled,
		CatalogSlug:    r.CatalogSlug,
		InstallStatus:  r.InstallStatus,
		CreatedAt:      r.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:      r.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if e.InstallStatus == "" {
		e.InstallStatus = InstallUnknown
	}
	if len(r.InstallResult) > 0 {
		_ = json.Unmarshal(r.InstallResult, &e.InstallResult)
	}
	if e.Transport == "" {
		e.Transport = TransportStdio
	}
	return e
}

// secretExists reports whether a tenant secret with that name is stored.
func secretExists(ctx context.Context, tx pgx.Tx, tenantID, name string) (bool, error) {
	_, err := db.GetSecretByName(ctx, tx, tenantID, name)
	if errors.Is(err, db.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ListForTenant returns every stored MCP server entry for the tenant,
// each with the required-secrets + has_secret_stored view (never values).
func (s *Service) ListForTenant(ctx context.Context, tenantID string) ([]Entry, error) {
	tx, err := s.begin(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := db.ListMCPServers(ctx, tx.Tx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(rows))
	for _, r := range rows {
		e := entryFromRow(r)
		e.RequiredSecrets = RequiredSecretsFor(e)
		if len(e.RequiredSecrets) > 0 {
			e.HasSecretStored, err = anySecretStored(ctx, tx.Tx, tenantID, e)
			if err != nil {
				return nil, err
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// ListForScope returns the definitions VISIBLE to a scope, owner-scoped:
// project-only when conversationID is empty, project ∪ conversation
// otherwise; both empty lists the whole tenant (preserving ListForTenant's
// behaviour for the unscoped callers).
func (s *Service) ListForScope(ctx context.Context, tenantID, projectID, conversationID string) ([]Entry, error) {
	projectID = strings.TrimSpace(projectID)
	conversationID = strings.TrimSpace(conversationID)
	if projectID == "" && conversationID == "" {
		return s.ListForTenant(ctx, tenantID)
	}
	tx, err := s.begin(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := db.ListMCPServersByOwner(ctx, tx.Tx, tenantID, projectID, conversationID)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(rows))
	for _, r := range rows {
		e := entryFromRow(r)
		e.RequiredSecrets = RequiredSecretsFor(e)
		if len(e.RequiredSecrets) > 0 {
			e.HasSecretStored, err = anySecretStored(ctx, tx.Tx, tenantID, e)
			if err != nil {
				return nil, err
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// anySecretStored reports whether ANY of the entry's credential refs
// (${NAME} in env/headers) resolves to an existing tenant secret.
func anySecretStored(ctx context.Context, tx pgx.Tx, tenantID string, e Entry) (bool, error) {
	for _, v := range e.Env {
		if name, ok := secretRefName(v); ok {
			exists, err := secretExists(ctx, tx, tenantID, name)
			if err != nil {
				return false, err
			}
			if exists {
				return true, nil
			}
		}
	}
	for _, v := range e.Headers {
		if name, ok := secretRefName(v); ok {
			exists, err := secretExists(ctx, tx, tenantID, name)
			if err != nil {
				return false, err
			}
			if exists {
				return true, nil
			}
		}
	}
	return false, nil
}

// RequiredSecretsFor returns the catalog-flagged secret env var names for
// an entry (union of catalog slug match + ${NAME} refs in env/headers).
func RequiredSecretsFor(e Entry) []string {
	seen := map[string]bool{}
	var out []string
	if e.CatalogSlug != "" {
		if c, ok := catalogBySlug(e.CatalogSlug); ok {
			for _, name := range c.RequiredEnv {
				if !seen[name] {
					seen[name] = true
					out = append(out, name)
				}
			}
		}
	}
	for _, v := range e.Env {
		if name, ok := secretRefName(v); ok && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, v := range e.Headers {
		if name, ok := secretRefName(v); ok && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// secretRefName extracts the secret name from a ${NAME} reference.
func secretRefName(v string) (string, bool) {
	if len(v) >= 3 && strings.HasPrefix(v, "${") && strings.HasSuffix(v, "}") {
		return v[2 : len(v)-1], true
	}
	return "", false
}

// SecretNameFor derives the standard tenant-secret name for a server's
// credential. Format: MCP_<SLUG>_<ENVNAME> where the slug component is the
// catalog slug uppercased, or the server id (ULID) uppercased for manual
// entries. Both satisfy ^[A-Z][A-Z0-9_]+$ and ≤ 64 chars.
func SecretNameFor(e Entry, envName string) string {
	comp := strings.ToUpper(e.CatalogSlug)
	if comp == "" {
		comp = strings.ToUpper(e.ID)
	}
	return "MCP_" + comp + "_" + strings.ToUpper(envName)
}

// validateSecretNameRe validates a derived secret name is storable.
func validateSecretNameRe(name string) error {
	if err := secrets.ValidateName(name); err != nil {
		return invalidf("%v", err)
	}
	return nil
}

func (s *Service) validateSecretRefs(ctx context.Context, tx pgx.Tx, tenantID string, env, headers map[string]string) error {
	for k, v := range env {
		if name, ok := secretRefName(v); ok {
			exists, err := secretExists(ctx, tx, tenantID, name)
			if err != nil {
				return err
			}
			if !exists {
				return invalidf("env %q references secret %q which does not exist in the tenant secrets store", k, name)
			}
		}
	}
	for k, v := range headers {
		if name, ok := secretRefName(v); ok {
			exists, err := secretExists(ctx, tx, tenantID, name)
			if err != nil {
				return err
			}
			if !exists {
				return invalidf("header %q references secret %q which does not exist in the tenant secrets store", k, name)
			}
		}
	}
	return nil
}

// validateEnvKeys enforces the env/header key grammar + bounds.
func validateMapKeys(m map[string]string, what string, maxEntries int) error {
	if len(m) > maxEntries {
		return invalidf("%s too many entries (max %d)", what, maxEntries)
	}
	for k, v := range m {
		if !envKeyRE.MatchString(k) {
			return invalidf("%s key %q must match %s", what, k, envKeyPattern)
		}
		if len(v) > maxCommandLen {
			return invalidf("%s value for %q too long (max %d)", what, k, maxCommandLen)
		}
	}
	return nil
}

func validateArgs(args []string) error {
	if len(args) > maxArgs {
		return invalidf("args too many (max %d)", maxArgs)
	}
	for _, a := range args {
		if len(a) > maxArgLen {
			return invalidf("arg too long (max %d)", maxArgLen)
		}
	}
	return nil
}

func validateURL(raw string) error {
	if raw == "" {
		return invalidf("url must not be empty")
	}
	if len(raw) > maxURLValueLen {
		return invalidf("url too long (max %d)", maxURLValueLen)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return invalidf("url must be an absolute http(s) URL")
	}
	return nil
}

// CreateInput is the writable create payload.
type CreateInput struct {
	Name string
	// ProjectID / ConversationID are the scope the definition is created
	// in. Exactly ONE must be non-empty (owner XOR); the scope is IMMUTABLE
	// after create.
	ProjectID      string
	ConversationID string
	Transport      string
	Command        string
	Args           []string
	Env            map[string]string
	URL            string
	Headers        map[string]string
	Enabled        bool
	CatalogSlug    string
}

func (s *Service) validateCreate(ctx context.Context, tx pgx.Tx, tenantID string, in *CreateInput) error {
	in.Name = strings.TrimSpace(in.Name)
	in.ProjectID = strings.TrimSpace(in.ProjectID)
	in.ConversationID = strings.TrimSpace(in.ConversationID)
	in.Command = strings.TrimSpace(in.Command)
	in.URL = strings.TrimSpace(in.URL)
	in.CatalogSlug = strings.TrimSpace(in.CatalogSlug)
	if in.Name == "" || len(in.Name) > maxNameLen {
		return invalidf("name must be 1–%d characters", maxNameLen)
	}
	// Owner XOR: a definition must belong to exactly one scope. Both set or
	// neither is a caller bug, not a default.
	switch {
	case in.ProjectID == "" && in.ConversationID == "":
		return invalidf("exactly one of project_id or conversation_id must be set")
	case in.ProjectID != "" && in.ConversationID != "":
		return invalidf("project_id and conversation_id are mutually exclusive")
	case in.ProjectID != "":
		if err := db.RequireProjectActive(ctx, tx, tenantID, in.ProjectID); err != nil {
			return err
		}
	default:
		if err := db.RequireConversation(ctx, tx, tenantID, in.ConversationID); err != nil {
			return err
		}
	}
	switch in.Transport {
	case "":
		in.Transport = TransportStdio
	case TransportStdio, TransportStreamable:
	default:
		return invalidf("transport must be %q or %q", TransportStdio, TransportStreamable)
	}
	if err := validateArgs(in.Args); err != nil {
		return err
	}
	if err := validateMapKeys(in.Env, "env", 64); err != nil {
		return err
	}
	if err := validateMapKeys(in.Headers, "headers", 32); err != nil {
		return err
	}
	if in.Transport == TransportStdio {
		if in.Command == "" {
			return invalidf("stdio servers require a command")
		}
		if len(in.Command) > maxCommandLen {
			return invalidf("command too long (max %d)", maxCommandLen)
		}
		in.URL = ""
		in.Headers = nil
	} else {
		if err := validateURL(in.URL); err != nil {
			return err
		}
		in.Command = ""
		in.Args = nil
		in.Env = nil
	}
	if in.CatalogSlug != "" {
		if _, ok := catalogBySlug(in.CatalogSlug); !ok {
			return invalidf("catalog_slug %q is not a known catalog entry", in.CatalogSlug)
		}
	}
	return s.validateSecretRefs(ctx, tx, tenantID, in.Env, in.Headers)
}

// Create stores a new MCP server entry.
func (s *Service) Create(ctx context.Context, tenantID string, in CreateInput) (Entry, error) {
	tx, err := s.begin(ctx, tenantID)
	if err != nil {
		return Entry{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.validateCreate(ctx, tx.Tx, tenantID, &in); err != nil {
		return Entry{}, err
	}
	// Name-uniqueness pre-check for a clean error. The uniqueness is
	// OWNER-SCOPED now (two projects may each own a `postgres`), and the
	// partial unique index is the backstop for the race the pre-check
	// cannot close.
	if _, err := db.GetMCPServerByNameForOwner(ctx, tx.Tx, tenantID, in.ProjectID, in.ConversationID, in.Name); err == nil {
		return Entry{}, invalidf("an MCP server named %q already exists in this scope", in.Name)
	} else if !errors.Is(err, db.ErrNotFound) {
		return Entry{}, err
	}
	row, err := db.UpsertMCPServer(ctx, tx.Tx, db.MCPServerRow{
		ID:             uuid.NewString(),
		TenantID:       tenantID,
		Name:           in.Name,
		ProjectID:      in.ProjectID,
		ConversationID: in.ConversationID,
		Transport:      in.Transport,
		Command:        in.Command,
		Args:           in.Args,
		Env:            in.Env,
		URL:            in.URL,
		Headers:        in.Headers,
		Enabled:        in.Enabled,
		CatalogSlug:    in.CatalogSlug,
		InstallStatus:  InstallUnknown,
	})
	if err != nil {
		return Entry{}, err
	}
	if err := audit.Record(ctx, tx.Tx, audit.Entry{TenantID: tenantID, Action: "mcp_server.created", TargetType: "mcp_server", TargetID: row.ID,
		After: audit.Snapshot(map[string]any{"name": row.Name, "transport": row.Transport})}); err != nil {
		return Entry{}, fmt.Errorf("mcpsettings: audit create: %w", err)
	}
	e := entryFromRow(row)
	e.RequiredSecrets = RequiredSecretsFor(e)
	if err := tx.Commit(ctx); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// Get returns one entry by id.
func (s *Service) Get(ctx context.Context, tenantID, id string) (Entry, error) {
	tx, err := s.begin(ctx, tenantID)
	if err != nil {
		return Entry{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	row, err := db.GetMCPServer(ctx, tx.Tx, tenantID, id)
	if err != nil {
		return Entry{}, err
	}
	e := entryFromRow(row)
	e.RequiredSecrets = RequiredSecretsFor(e)
	if len(e.RequiredSecrets) > 0 {
		e.HasSecretStored, err = anySecretStored(ctx, tx.Tx, tenantID, e)
		if err != nil {
			return Entry{}, err
		}
	}
	return e, nil
}

// UpdateInput is the partial-update merge (optional-pointer pattern).
type UpdateInput struct {
	ID string
	// ProjectID / ConversationID may be echoed by a client; a value that
	// DIFFERS from the stored owner is rejected (owner is immutable).
	ProjectID      string
	ConversationID string

	Name           *string
	Transport      *string
	Command        *string
	Args           []string
	ReplaceArgs    bool
	Env            map[string]string
	ReplaceEnv     bool
	URL            *string
	Headers        map[string]string
	ReplaceHeaders bool
	Enabled        *bool
	CatalogSlug    *string
}

// Update applies a partial update. The name is immutable after create.
func (s *Service) Update(ctx context.Context, tenantID string, in UpdateInput) (Entry, error) {
	in.ID = strings.TrimSpace(in.ID)
	if in.ID == "" {
		return Entry{}, invalidf("id must not be empty")
	}
	tx, err := s.begin(ctx, tenantID)
	if err != nil {
		return Entry{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	existing, err := db.GetMCPServer(ctx, tx.Tx, tenantID, in.ID)
	if err != nil {
		return Entry{}, err
	}
	merged := existing
	if in.Name != nil {
		return Entry{}, invalidf("name is immutable after create; delete + recreate to rename")
	}
	// The SCOPE is immutable after create too (like the name): moving a
	// definition is a different row semantically, and an owner swap would
	// race the owner-scoped partial unique indexes.
	if strings.TrimSpace(in.ProjectID) != "" && strings.TrimSpace(in.ProjectID) != existing.ProjectID {
		return Entry{}, invalidf("owner is immutable after create; delete + recreate to move")
	}
	if strings.TrimSpace(in.ConversationID) != "" && strings.TrimSpace(in.ConversationID) != existing.ConversationID {
		return Entry{}, invalidf("owner is immutable after create; delete + recreate to move")
	}
	if in.Transport != nil {
		merged.Transport = strings.TrimSpace(*in.Transport)
	}
	if in.Command != nil {
		merged.Command = strings.TrimSpace(*in.Command)
	}
	if in.ReplaceArgs {
		merged.Args = in.Args
	}
	if in.ReplaceEnv {
		merged.Env = in.Env
	} else if in.Env != nil {
		if merged.Env == nil {
			merged.Env = map[string]string{}
		}
		for k, v := range in.Env {
			merged.Env[k] = v
		}
	}
	if in.URL != nil {
		merged.URL = strings.TrimSpace(*in.URL)
	}
	if in.ReplaceHeaders {
		merged.Headers = in.Headers
	} else if in.Headers != nil {
		if merged.Headers == nil {
			merged.Headers = map[string]string{}
		}
		for k, v := range in.Headers {
			merged.Headers[k] = v
		}
	}
	if in.Enabled != nil {
		merged.Enabled = *in.Enabled
	}
	if in.CatalogSlug != nil {
		merged.CatalogSlug = strings.TrimSpace(*in.CatalogSlug)
	}

	// Re-validate the merged shape through the create validator. The OWNER is
	// carried through: the validator requires exactly one owner, and the
	// merged row already has it (it is immutable, so it equals the stored
	// one).
	chk := CreateInput{
		Name:           merged.Name,
		ProjectID:      merged.ProjectID,
		ConversationID: merged.ConversationID,
		Transport:      merged.Transport,
		Command:        merged.Command,
		Args:           merged.Args,
		Env:            merged.Env,
		URL:            merged.URL,
		Headers:        merged.Headers,
		Enabled:        merged.Enabled,
		CatalogSlug:    merged.CatalogSlug,
	}
	if err := s.validateCreate(ctx, tx.Tx, tenantID, &chk); err != nil {
		return Entry{}, err
	}
	merged.Transport = chk.Transport
	merged.Command = chk.Command
	merged.Args = chk.Args
	merged.Env = chk.Env
	merged.URL = chk.URL
	merged.Headers = chk.Headers
	merged.CatalogSlug = chk.CatalogSlug

	row, err := db.UpsertMCPServer(ctx, tx.Tx, merged)
	if err != nil {
		return Entry{}, err
	}
	if err := audit.Record(ctx, tx.Tx, audit.Entry{TenantID: tenantID, Action: "mcp_server.updated", TargetType: "mcp_server", TargetID: row.ID,
		After: audit.Snapshot(map[string]any{"name": row.Name, "transport": row.Transport})}); err != nil {
		return Entry{}, fmt.Errorf("mcpsettings: audit update: %w", err)
	}
	e := entryFromRow(row)
	e.RequiredSecrets = RequiredSecretsFor(e)
	if err := tx.Commit(ctx); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// Delete removes an entry. There is NO reference guard: an owner-scoped
// definition cannot be orphaned (its owner IS its only reference, and both
// owner FKs cascade). Derived secrets are purged in the same tx.
func (s *Service) Delete(ctx context.Context, tenantID, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return invalidf("id must not be empty")
	}
	tx, err := s.begin(ctx, tenantID)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	existing, err := db.GetMCPServer(ctx, tx.Tx, tenantID, id)
	if err != nil {
		return err
	}
	// NO REFERENCE GUARD: an owner-scoped definition cannot be orphaned —
	// every reference IS the row's owner (mcp_servers.project_id /
	// conversation_id, with ON DELETE CASCADE on both FKs). The guard and
	// its ReferencedError existed only because a definition could be
	// SHARED, which the owner-scoped model removes.
	//
	// Purge derived secrets: delete where name LIKE 'MCP_<slug>_%'.
	comp := strings.ToUpper(existing.CatalogSlug)
	if comp == "" {
		comp = strings.ToUpper(existing.ID)
	}
	prefix := "MCP_" + comp + "_%"
	secRows, err := tx.Query(ctx,
		`SELECT id FROM tenant_secrets WHERE tenant_id=$1 AND name LIKE $2`, tenantID, prefix)
	if err != nil {
		return err
	}
	var secIDs []string
	for secRows.Next() {
		var sid string
		if err := secRows.Scan(&sid); err != nil {
			secRows.Close()
			return err
		}
		secIDs = append(secIDs, sid)
	}
	secRows.Close()
	if err := secRows.Err(); err != nil {
		return err
	}
	for _, sid := range secIDs {
		if err := db.DeleteSecret(ctx, tx.Tx, tenantID, sid); err != nil && !errors.Is(err, db.ErrNotFound) {
			return err
		}
	}
	if err := db.DeleteMCPServer(ctx, tx.Tx, tenantID, id); err != nil {
		return err
	}
	if err := audit.Record(ctx, tx.Tx, audit.Entry{TenantID: tenantID, Action: "mcp_server.deleted", TargetType: "mcp_server", TargetID: id}); err != nil {
		return fmt.Errorf("mcpsettings: audit delete: %w", err)
	}
	return tx.Commit(ctx)
}

// SetSecret writes/updates a tenant secret for an entry's credential and
// points the env/headers reference at ${NAME} (write-only; plaintext never
// returned). name must be an env/header key or a required-secret name.
func (s *Service) SetSecret(ctx context.Context, tenantID, id, name, value string) (secretName string, err error) {
	name = strings.TrimSpace(name)
	value = strings.TrimSpace(value)
	if name == "" {
		return "", invalidf("name must not be empty")
	}
	if value == "" {
		return "", invalidf("value must not be empty")
	}
	if len(value) > 4096 {
		return "", invalidf("value too long (max 4096)")
	}
	if !envKeyRE.MatchString(name) {
		return "", invalidf("name %q must match %s", name, envKeyPattern)
	}
	if err := s.requireKEK(); err != nil {
		return "", err
	}
	tx, err := s.begin(ctx, tenantID)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	row, err := db.GetMCPServer(ctx, tx.Tx, tenantID, id)
	if err != nil {
		return "", err
	}
	e := entryFromRow(row)
	// The credential key must be part of the entry: a required secret or an
	// existing env/header key (manual entries: the operator adds the key to
	// the form first).
	allowed := false
	for _, rs := range RequiredSecretsFor(e) {
		if rs == name {
			allowed = true
			break
		}
	}
	if !allowed {
		if _, ok := e.Env[name]; ok {
			allowed = true
		}
		if _, ok := e.Headers[name]; ok {
			allowed = true
		}
	}
	if !allowed {
		return "", invalidf("%q is not an env/header key of this server and not a required secret; add it to the server config first", name)
	}
	secretName = SecretNameFor(e, name)
	if err := validateSecretNameRe(secretName); err != nil {
		return "", err
	}
	ct, err := secretcrypto.Encrypt([]byte(value), s.kek)
	if err != nil {
		return "", err
	}
	existingSec, serr := db.GetSecretByName(ctx, tx.Tx, tenantID, secretName)
	switch {
	case serr == nil:
		kv := int16(1)
		if _, err := db.UpdateSecret(ctx, tx.Tx, tenantID, existingSec.ID, nil, &ct, &kv); err != nil {
			return "", err
		}
	case errors.Is(serr, db.ErrNotFound):
		if _, err := db.CreateSecret(ctx, tx.Tx, db.SecretRow{
			ID: uuid.NewString(), TenantID: tenantID, Name: secretName,
			Description: "MCP server credential (auto-written from Settings → Adapters → MCP)",
			Ciphertext:  ct, KeyVersion: 1,
		}); err != nil {
			return "", err
		}
	default:
		return "", serr
	}
	// Point the env/header reference at ${NAME}.
	if _, inEnv := e.Env[name]; inEnv {
		env := cloneMap(e.Env)
		env[name] = "${" + secretName + "}"
		if err := db.UpdateMCPServerConfig(ctx, tx.Tx, tenantID, id, env, e.Headers); err != nil {
			return "", err
		}
	} else if _, inHdr := e.Headers[name]; inHdr {
		hdrs := cloneMap(e.Headers)
		hdrs[name] = "${" + secretName + "}"
		if err := db.UpdateMCPServerConfig(ctx, tx.Tx, tenantID, id, e.Env, hdrs); err != nil {
			return "", err
		}
	} else {
		// Required secret with no env key yet: add it to env as a reference.
		env := cloneMap(e.Env)
		env[name] = "${" + secretName + "}"
		if err := db.UpdateMCPServerConfig(ctx, tx.Tx, tenantID, id, env, e.Headers); err != nil {
			return "", err
		}
	}
	if err := audit.Record(ctx, tx.Tx, audit.Entry{TenantID: tenantID, Action: "mcp_server.secret_set", TargetType: "mcp_server", TargetID: id,
		After: audit.Snapshot(map[string]any{"secret": secretName})}); err != nil {
		return "", fmt.Errorf("mcpsettings: audit secret set: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return secretName, nil
}

// ClearSecret deletes the stored tenant secret and removes the ${NAME}
// reference from env/headers. Idempotent.
func (s *Service) ClearSecret(ctx context.Context, tenantID, id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return invalidf("name must not be empty")
	}
	tx, err := s.begin(ctx, tenantID)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	row, err := db.GetMCPServer(ctx, tx.Tx, tenantID, id)
	if err != nil {
		return err
	}
	e := entryFromRow(row)
	secretName := SecretNameFor(e, name)
	if sec, serr := db.GetSecretByName(ctx, tx.Tx, tenantID, secretName); serr == nil {
		if err := db.DeleteSecret(ctx, tx.Tx, tenantID, sec.ID); err != nil && !errors.Is(err, db.ErrNotFound) {
			return err
		}
	} else if !errors.Is(serr, db.ErrNotFound) {
		return serr
	}
	env := cloneMap(e.Env)
	hdrs := cloneMap(e.Headers)
	if _, ok := env[name]; ok {
		env[name] = ""
		delete(env, name)
	}
	if _, ok := hdrs[name]; ok {
		hdrs[name] = ""
		delete(hdrs, name)
	}
	if err := db.UpdateMCPServerConfig(ctx, tx.Tx, tenantID, id, env, hdrs); err != nil {
		return err
	}
	if err := audit.Record(ctx, tx.Tx, audit.Entry{TenantID: tenantID, Action: "mcp_server.secret_cleared", TargetType: "mcp_server", TargetID: id}); err != nil {
		return fmt.Errorf("mcpsettings: audit secret clear: %w", err)
	}
	return tx.Commit(ctx)
}

func dedupIDs(ids []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func cloneMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
