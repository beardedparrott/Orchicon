package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	apiv1connect "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Service implements the AuthService Connect handler
// (docs/07_API_Specification.md §3.12). It manages API keys (hashed,
// scoped entitlements), identity info, RBAC roles + role bindings, the
// tenants list, and the audit view.
type Service struct {
	pool *db.Pool
	log  *slog.Logger
	apiv1connect.UnimplementedAuthServiceHandler
}

// Compile-time assertion that Service satisfies the handler interface.
var _ apiv1connect.AuthServiceHandler = (*Service)(nil)

// NewService constructs an AuthService handler.
func NewService(pool *db.Pool, log *slog.Logger) *Service {
	return &Service{pool: pool, log: log}
}

// --- API keys --------------------------------------------------------------

// CreateApiKey mints a new hashed API key with scoped entitlements.
// The plaintext key is returned exactly once (ApiKeySecret); only the
// hash is persisted (AGENTS.md security standards: hashed at rest).
func (s *Service) CreateApiKey(ctx context.Context, req *connect.Request[apiv1.CreateApiKeyRequest]) (*connect.Response[apiv1.CreateApiKeyResponse], error) {
	msg := req.Msg
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	name := strings.TrimSpace(msg.Name)
	if name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name must not be empty"))
	}
	if len(name) > 200 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name too long"))
	}
	if msg.IdentityId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("identity_id must not be empty"))
	}
	scopes, err := validateEntitlements(msg.Scopes)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	plaintext, prefix, hash := GenerateApiKey()
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	// Verify the identity exists.
	if _, err := db.GetIdentity(ctx, ttx.Tx, tenantID, msg.IdentityId); err != nil {
		return nil, mapDBError(err)
	}
	row, err := db.CreateApiKey(ctx, ttx.Tx, db.ApiKeyRow{
		TenantID:   tenantID,
		IdentityID: msg.IdentityId,
		Name:       name,
		KeyPrefix:  prefix,
		KeyHash:    hash,
		Scopes:     scopes,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("api key created", "id", row.ID, "identity", msg.IdentityId, "tenant", tenantID)
	return connect.NewResponse(&apiv1.CreateApiKeyResponse{
		ApiKey: apiKeyRowToProto(row),
		Secret: &apiv1.ApiKeySecret{Id: row.ID, Key: plaintext},
	}), nil
}

// RevokeApiKey transitions an API key to revoked status.
func (s *Service) RevokeApiKey(ctx context.Context, req *connect.Request[apiv1.RevokeApiKeyRequest]) (*connect.Response[apiv1.RevokeApiKeyResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetApiKey(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	row, err := db.UpdateApiKeyStatus(ctx, ttx.Tx, tenantID, req.Msg.Id, "revoked", current.Version)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	return connect.NewResponse(&apiv1.RevokeApiKeyResponse{ApiKey: apiKeyRowToProto(row)}), nil
}

// RotateApiKey issues a new plaintext + hash for an existing key.
func (s *Service) RotateApiKey(ctx context.Context, req *connect.Request[apiv1.RotateApiKeyRequest]) (*connect.Response[apiv1.RotateApiKeyResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	plaintext, prefix, hash := GenerateApiKey()
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetApiKey(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	row, err := db.RotateApiKeyHash(ctx, ttx.Tx, tenantID, req.Msg.Id, prefix, hash, current.Version)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("api key rotated", "id", row.ID)
	return connect.NewResponse(&apiv1.RotateApiKeyResponse{
		ApiKey: apiKeyRowToProto(row),
		Secret: &apiv1.ApiKeySecret{Id: row.ID, Key: plaintext},
	}), nil
}

// ListApiKeys returns a page of API keys.
func (s *Service) ListApiKeys(ctx context.Context, req *connect.Request[apiv1.ListApiKeysRequest]) (*connect.Response[apiv1.ListApiKeysResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	rows, err := db.ListApiKeys(ctx, ttx.Tx, tenantID, req.Msg.IdentityId, int(req.Msg.PageSize), req.Msg.PageToken)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	resp := &apiv1.ListApiKeysResponse{}
	for _, r := range rows {
		resp.ApiKeys = append(resp.ApiKeys, apiKeyRowToProto(r))
	}
	if len(rows) > 0 {
		resp.NextPageToken = rows[len(rows)-1].ID
	}
	return connect.NewResponse(resp), nil
}

// GetApiKey returns a single API key by id.
func (s *Service) GetApiKey(ctx context.Context, req *connect.Request[apiv1.GetApiKeyRequest]) (*connect.Response[apiv1.GetApiKeyResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	row, err := db.GetApiKey(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	return connect.NewResponse(&apiv1.GetApiKeyResponse{ApiKey: apiKeyRowToProto(row)}), nil
}

// --- Identity + entitlements ------------------------------------------------

// GetIdentity returns a single identity by id.
func (s *Service) GetIdentity(ctx context.Context, req *connect.Request[apiv1.GetIdentityRequest]) (*connect.Response[apiv1.GetIdentityResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	row, err := db.GetIdentity(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	return connect.NewResponse(&apiv1.GetIdentityResponse{Identity: identityRowToProto(row)}), nil
}

// ListIdentities returns a page of identities.
func (s *Service) ListIdentities(ctx context.Context, req *connect.Request[apiv1.ListIdentitiesRequest]) (*connect.Response[apiv1.ListIdentitiesResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	rows, err := db.ListIdentities(ctx, ttx.Tx, db.ListIdentitiesFilter{
		TenantID: tenantID, PageSize: int(req.Msg.PageSize), AfterID: req.Msg.PageToken,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	resp := &apiv1.ListIdentitiesResponse{}
	for _, r := range rows {
		resp.Identities = append(resp.Identities, identityRowToProto(r))
	}
	if len(rows) > 0 {
		resp.NextPageToken = rows[len(rows)-1].ID
	}
	return connect.NewResponse(resp), nil
}

// ListEntitlements returns the entitlements granted to an identity.
func (s *Service) ListEntitlements(ctx context.Context, req *connect.Request[apiv1.ListEntitlementsRequest]) (*connect.Response[apiv1.ListEntitlementsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.IdentityId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("identity_id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	ents, isAdmin, err := db.ListIdentityEntitlements(ctx, ttx.Tx, tenantID, req.Msg.IdentityId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	resp := &apiv1.ListEntitlementsResponse{IsAdmin: isAdmin}
	for _, e := range ents {
		parts := strings.SplitN(e, ":", 2)
		ent := &apiv1.Entitlement{}
		if len(parts) == 2 {
			ent.Resource = parts[0]
			ent.Action = parts[1]
		} else {
			ent.Resource = e
		}
		resp.Entitlements = append(resp.Entitlements, ent)
	}
	return connect.NewResponse(resp), nil
}

// --- RBAC roles + bindings -------------------------------------------------

// CreateRole creates a new RBAC role.
func (s *Service) CreateRole(ctx context.Context, req *connect.Request[apiv1.CreateRoleRequest]) (*connect.Response[apiv1.CreateRoleResponse], error) {
	msg := req.Msg
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	name := strings.TrimSpace(msg.Name)
	if name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name must not be empty"))
	}
	if !roleNameRE.MatchString(name) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("name must match %s", roleNameRE.String()))
	}
	scope := msg.Scope
	if scope == "" {
		scope = "tenant"
	}
	if scope != "tenant" && scope != "project" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("scope must be tenant or project"))
	}
	ents, err := validateEntitlements(msg.Entitlements)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	row, err := db.CreateRole(ctx, ttx.Tx, db.RoleRow{
		TenantID: tenantID, Name: name, Scope: scope, ScopeRef: msg.ScopeRef, Entitlements: ents,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	return connect.NewResponse(&apiv1.CreateRoleResponse{Role: roleRowToProto(row)}), nil
}

// ListRoles returns a page of roles.
func (s *Service) ListRoles(ctx context.Context, req *connect.Request[apiv1.ListRolesRequest]) (*connect.Response[apiv1.ListRolesResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	rows, err := db.ListRoles(ctx, ttx.Tx, tenantID, int(req.Msg.PageSize), req.Msg.PageToken)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	resp := &apiv1.ListRolesResponse{}
	for _, r := range rows {
		resp.Roles = append(resp.Roles, roleRowToProto(r))
	}
	if len(rows) > 0 {
		resp.NextPageToken = rows[len(rows)-1].ID
	}
	return connect.NewResponse(resp), nil
}

// AssignRole binds a role to an identity.
func (s *Service) AssignRole(ctx context.Context, req *connect.Request[apiv1.AssignRoleRequest]) (*connect.Response[apiv1.AssignRoleResponse], error) {
	msg := req.Msg
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if msg.IdentityId == "" || msg.RoleId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("identity_id and role_id required"))
	}
	scope := msg.Scope
	if scope == "" {
		scope = "tenant"
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	// Verify role + identity exist.
	if _, err := db.GetRole(ctx, ttx.Tx, tenantID, msg.RoleId); err != nil {
		return nil, mapDBError(err)
	}
	if _, err := db.GetIdentity(ctx, ttx.Tx, tenantID, msg.IdentityId); err != nil {
		return nil, mapDBError(err)
	}
	binding, err := db.CreateRoleBinding(ctx, ttx.Tx, db.RoleBindingRow{
		TenantID: tenantID, IdentityID: msg.IdentityId, RoleID: msg.RoleId, Scope: scope, ScopeRef: msg.ScopeRef,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	return connect.NewResponse(&apiv1.AssignRoleResponse{Binding: bindingRowToProto(binding)}), nil
}

// RevokeRole removes a role binding.
func (s *Service) RevokeRole(ctx context.Context, req *connect.Request[apiv1.RevokeRoleRequest]) (*connect.Response[apiv1.RevokeRoleResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	if err := db.DeleteRoleBinding(ctx, ttx.Tx, tenantID, req.Msg.Id); err != nil {
		return nil, mapDBError(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	return connect.NewResponse(&apiv1.RevokeRoleResponse{}), nil
}

// ListRoleBindings returns a page of role bindings.
func (s *Service) ListRoleBindings(ctx context.Context, req *connect.Request[apiv1.ListRoleBindingsRequest]) (*connect.Response[apiv1.ListRoleBindingsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	rows, err := db.ListRoleBindings(ctx, ttx.Tx, tenantID, req.Msg.IdentityId, int(req.Msg.PageSize), req.Msg.PageToken)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	resp := &apiv1.ListRoleBindingsResponse{}
	for _, r := range rows {
		resp.Bindings = append(resp.Bindings, bindingRowToProto(r))
	}
	if len(rows) > 0 {
		resp.NextPageToken = rows[len(rows)-1].ID
	}
	return connect.NewResponse(resp), nil
}

// --- Tenants (admin) -------------------------------------------------------

// ListTenants returns a page of tenants. The tenants table has no
// tenant_id (it IS the tenant — docs/09 §3.1); this is the one admin
// read that crosses tenants. The RBAC interceptor gates this to admin
// identities (auth:read).
func (s *Service) ListTenants(ctx context.Context, req *connect.Request[apiv1.ListTenantsRequest]) (*connect.Response[apiv1.ListTenantsResponse], error) {
	rows, err := db.ListTenants(ctx, s.pool, int(req.Msg.PageSize), req.Msg.PageToken)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	resp := &apiv1.ListTenantsResponse{}
	for _, r := range rows {
		resp.Tenants = append(resp.Tenants, tenantRowToProto(r))
	}
	if len(rows) > 0 {
		resp.NextPageToken = rows[len(rows)-1].ID
	}
	return connect.NewResponse(resp), nil
}

// CreateTenant provisions a new tenant. Admin-only path (the RBAC
// interceptor enforces auth:write + tenant:create before this is
// reached). Slug is validated server-side; the same regex the project
// service uses ([a-z0-9]+(?:-[a-z0-9]+)*) so URLs and identifiers
// stay consistent across the codebase.
func (s *Service) CreateTenant(ctx context.Context, req *connect.Request[apiv1.CreateTenantRequest]) (*connect.Response[apiv1.CreateTenantResponse], error) {
	msg := req.Msg
	slug := strings.TrimSpace(msg.Slug)
	if slug == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("slug must not be empty"))
	}
	if !slugRE.MatchString(slug) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("slug must match ^[a-z0-9]+(?:-[a-z0-9]+)*$"))
	}
	if len(slug) > 63 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("slug too long"))
	}
	name := strings.TrimSpace(msg.Name)
	if name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name must not be empty"))
	}
	if utf8.RuneCountInString(name) > 200 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name too long"))
	}
	// budget_envelope_json: optional, but if supplied must be valid JSON
	// so we don't store garbage in a jsonb column.
	budget := strings.TrimSpace(msg.BudgetEnvelopeJson)
	if budget != "" && !json.Valid([]byte(budget)) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("budget_envelope_json is not valid JSON"))
	}
	row, err := db.CreateTenant(ctx, s.pool, slug, name, budget)
	if err != nil {
		// Most likely a unique-constraint violation on slug.
		if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "unique constraint") {
			return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("tenant slug %q already in use", slug))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	s.log.Info("tenant created", "id", row.ID, "slug", row.Slug, "name", row.Name)
	return connect.NewResponse(&apiv1.CreateTenantResponse{Tenant: tenantRowToProto(row)}), nil
}

// --- Audit -----------------------------------------------------------------

// ListAuditEntries returns a page of policy decisions (the audit view).
func (s *Service) ListAuditEntries(ctx context.Context, req *connect.Request[apiv1.ListAuditEntriesRequest]) (*connect.Response[apiv1.ListAuditEntriesResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	pageSize := int(req.Msg.PageSize)
	if pageSize <= 0 || pageSize > 1000 {
		pageSize = 100
	}
	q := `SELECT id, tenant_id, decision_point, effect, actor_type, actor_id,
		target_type, target_id, trace_id, error, occurred_at
		FROM policy_decisions
		WHERE tenant_id = $1 AND ($2 = '' OR decision_point = $2) AND ($3 = '' OR actor_id = $3)
			AND ($4 = '' OR id > $4)
		ORDER BY occurred_at DESC LIMIT $5`
	rows, err := s.pool.Query(ctx, q, tenantID, req.Msg.DecisionPoint, req.Msg.ActorId, req.Msg.PageToken, pageSize)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer rows.Close()
	resp := &apiv1.ListAuditEntriesResponse{}
	for rows.Next() {
		var e struct {
			ID            string
			TenantID      string
			DecisionPoint string
			Effect        string
			ActorType     string
			ActorID       string
			TargetType    string
			TargetID      string
			TraceID       string
			Error         string
			OccurredAt    time.Time
		}
		if err := rows.Scan(&e.ID, &e.TenantID, &e.DecisionPoint, &e.Effect, &e.ActorType,
			&e.ActorID, &e.TargetType, &e.TargetID, &e.TraceID, &e.Error, &e.OccurredAt); err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("scan audit: %w", err))
		}
		resp.Entries = append(resp.Entries, &apiv1.AuditEntry{
			Id:            e.ID,
			TenantId:      e.TenantID,
			DecisionPoint: e.DecisionPoint,
			Effect:        e.Effect,
			ActorType:     e.ActorType,
			ActorId:       e.ActorID,
			TargetType:    e.TargetType,
			TargetId:      e.TargetID,
			TraceId:       e.TraceID,
			Error:         e.Error,
			OccurredAt:    timestamppb.New(e.OccurredAt),
		})
	}
	return connect.NewResponse(resp), rows.Err()
}

// --- helpers ---------------------------------------------------------------

func requireTenant(ctx context.Context) (string, error) {
	id := tenant.FromContext(ctx)
	if id == "" {
		return "", errors.New("no tenant in context")
	}
	return id, nil
}

func mapDBError(err error) error {
	if errors.Is(err, db.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, errors.New("not found"))
	}
	return connect.NewError(connect.CodeInternal, err)
}

var roleNameRE = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// validateEntitlements normalizes + bounds-checks a list of
// "resource:action" entitlements.
func validateEntitlements(ents []string) ([]string, error) {
	if len(ents) > 200 {
		return nil, errors.New("too many entitlements")
	}
	out := make([]string, 0, len(ents))
	seen := map[string]struct{}{}
	for _, e := range ents {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if len(e) > 64 {
			return nil, fmt.Errorf("entitlement too long: %s", e)
		}
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	return out, nil
}

func identityRowToProto(r db.IdentityRow) *apiv1.Identity {
	return &apiv1.Identity{
		Id:           r.ID,
		TenantId:     r.TenantID,
		Subject:      r.Subject,
		DisplayName:  nullableStr(r.DisplayName),
		Status:       r.Status,
		IdentityType: r.IdentityType,
		Version:      int32(r.Version),
		CreatedAt:    timestamppb.New(r.CreatedAt),
		UpdatedAt:    timestamppb.New(r.UpdatedAt),
	}
}

func roleRowToProto(r db.RoleRow) *apiv1.Role {
	return &apiv1.Role{
		Id:           r.ID,
		TenantId:     r.TenantID,
		Name:         r.Name,
		Scope:        r.Scope,
		ScopeRef:     r.ScopeRef,
		Entitlements: r.Entitlements,
		Version:      int32(r.Version),
		CreatedAt:    timestamppb.New(r.CreatedAt),
		UpdatedAt:    timestamppb.New(r.UpdatedAt),
	}
}

func bindingRowToProto(r db.RoleBindingRow) *apiv1.RoleBinding {
	return &apiv1.RoleBinding{
		Id:         r.ID,
		TenantId:   r.TenantID,
		IdentityId: r.IdentityID,
		RoleId:     r.RoleID,
		Scope:      r.Scope,
		ScopeRef:   r.ScopeRef,
		CreatedAt:  timestamppb.New(r.CreatedAt),
	}
}

func apiKeyRowToProto(r db.ApiKeyRow) *apiv1.ApiKey {
	lastUsed := (*timestamppb.Timestamp)(nil)
	if r.LastUsedAt != nil {
		lastUsed = timestamppb.New(*r.LastUsedAt)
	}
	return &apiv1.ApiKey{
		Id:         r.ID,
		TenantId:   r.TenantID,
		IdentityId: r.IdentityID,
		Name:       r.Name,
		Prefix:     r.KeyPrefix,
		Scopes:     r.Scopes,
		Status:     r.Status,
		LastUsedAt: lastUsed,
		Version:    int32(r.Version),
		CreatedAt:  timestamppb.New(r.CreatedAt),
		UpdatedAt:  timestamppb.New(r.UpdatedAt),
	}
}

func tenantRowToProto(r db.TenantRow) *apiv1.Tenant {
	return &apiv1.Tenant{
		Id:        r.ID,
		Slug:      r.Slug,
		Name:      r.Name,
		Status:    r.Status,
		Version:   int32(r.Version),
		CreatedAt: timestamppb.New(r.CreatedAt),
		UpdatedAt: timestamppb.New(r.UpdatedAt),
	}
}

func nullableStr(s string) string { return s }

// pgx import retained for future direct queries.
var _ = pgx.ErrNoRows
var _ = json.Valid

// slugRE defines the canonical slug character set: lowercase
// alphanumerics and hyphens, must start and end alphanumeric. Mirrors
// the project service's slug regex so tenant and project slugs follow
// the same rules (a tenant's slug becomes the path prefix for the
// projects, workers, etc. nested under it).
var slugRE = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
