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
	"github.com/beardedparrott/orchicon/internal/audit"
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
	actor := ActorFromContext(ctx)
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
	if err := audit.Record(ctx, ttx.Tx, audit.Entry{
		TenantID:        tenantID,
		ActorIdentityID: actor.ActorIdentityID,
		ActorType:       actor.ActorType,
		AuthMethod:      actor.AuthMethod,
		Action:          "api_key.created",
		TargetType:      "api_key",
		TargetID:        row.ID,
		After:           audit.Snapshot(apiKeyAuditSnapshot(row)),
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit api_key.created: %w", err))
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
	actor := ActorFromContext(ctx)
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
	if err := audit.Record(ctx, ttx.Tx, audit.Entry{
		TenantID:        tenantID,
		ActorIdentityID: actor.ActorIdentityID,
		ActorType:       actor.ActorType,
		AuthMethod:      actor.AuthMethod,
		Action:          "api_key.revoked",
		TargetType:      "api_key",
		TargetID:        row.ID,
		Before:          audit.SnapshotStatus(current.Status),
		After:           audit.SnapshotStatus(row.Status),
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit api_key.revoked: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	return connect.NewResponse(&apiv1.RevokeApiKeyResponse{ApiKey: apiKeyRowToProto(row)}), nil
}

// RotateApiKey issues a new plaintext + hash for an existing key.
func (s *Service) RotateApiKey(ctx context.Context, req *connect.Request[apiv1.RotateApiKeyRequest]) (*connect.Response[apiv1.RotateApiKeyResponse], error) {
	actor := ActorFromContext(ctx)
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
	if err := audit.Record(ctx, ttx.Tx, audit.Entry{
		TenantID:        tenantID,
		ActorIdentityID: actor.ActorIdentityID,
		ActorType:       actor.ActorType,
		AuthMethod:      actor.AuthMethod,
		Action:          "api_key.rotated",
		TargetType:      "api_key",
		TargetID:        row.ID,
		Before:          audit.SnapshotStatus(current.Status),
		After:           audit.SnapshotStatus(row.Status),
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit api_key.rotated: %w", err))
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

// serviceAccountSubjectRE bounds an explicitly-supplied service-account
// subject to the slug charset (mirrors the project/tenant slug rule so
// machine identifiers stay URL-safe and predictable).
var serviceAccountSubjectRE = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// CreateIdentity provisions a new identity (user or service account) in
// the caller's tenant. The RBAC interceptor gates this to auth:write.
// For identity_type "user" the subject is the login handle and must match
// the local-account username charset so the identity can immediately get
// a SetLocalCredential whose username matches its subject. For "service"
// the subject is optional; when omitted the server generates a synthetic
// "sa-<ULID>" subject.
func (s *Service) CreateIdentity(ctx context.Context, req *connect.Request[apiv1.CreateIdentityRequest]) (*connect.Response[apiv1.CreateIdentityResponse], error) {
	msg := req.Msg
	actor := ActorFromContext(ctx)
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	displayName := strings.TrimSpace(msg.DisplayName)
	if displayName == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("display_name must not be empty"))
	}
	if utf8.RuneCountInString(displayName) > 200 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("display_name too long"))
	}
	identityType := msg.IdentityType
	if identityType == "" {
		identityType = "user"
	}
	if identityType != "user" && identityType != "service" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("identity_type must be \"user\" or \"service\""))
	}
	subject := strings.TrimSpace(msg.Subject)
	switch identityType {
	case "user":
		if subject == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("subject must not be empty for user identities"))
		}
		if len(subject) > 255 {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("subject too long"))
		}
		if !localUsernameRE.MatchString(subject) {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("subject must match ^[a-z0-9][a-z0-9._@+-]*$"))
		}
	case "service":
		if subject != "" {
			if len(subject) > 63 {
				return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("subject too long"))
			}
			if !serviceAccountSubjectRE.MatchString(subject) {
				return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("subject must match ^[a-z0-9]+(?:-[a-z0-9]+)*$"))
			}
		} else {
			subject = "sa-" + db.NewID()
		}
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	row, err := db.CreateIdentity(ctx, ttx.Tx, db.IdentityRow{
		TenantID:     tenantID,
		Subject:      subject,
		DisplayName:  displayName,
		IdentityType: identityType,
	})
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "unique constraint") {
			return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("subject %q already in use", subject))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := audit.Record(ctx, ttx.Tx, audit.Entry{
		TenantID:        tenantID,
		ActorIdentityID: actor.ActorIdentityID,
		ActorType:       actor.ActorType,
		AuthMethod:      actor.AuthMethod,
		Action:          "identity.created",
		TargetType:      "identity",
		TargetID:        row.ID,
		After:           audit.Snapshot(identityAuditSnapshot(row)),
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit identity.created: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("identity created", "id", row.ID, "subject", row.Subject, "type", row.IdentityType, "tenant", tenantID)
	return connect.NewResponse(&apiv1.CreateIdentityResponse{Identity: identityRowToProto(row)}), nil
}

// UpdateIdentity edits an identity's display_name with optional
// optimistic concurrency. A version supplied on the request must match
// the current row or the update fails with NotFound (mirrors
// UpdateApiKeyStatus semantics).
func (s *Service) UpdateIdentity(ctx context.Context, req *connect.Request[apiv1.UpdateIdentityRequest]) (*connect.Response[apiv1.UpdateIdentityResponse], error) {
	msg := req.Msg
	actor := ActorFromContext(ctx)
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	displayName := strings.TrimSpace(msg.DisplayName)
	if displayName == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("display_name must not be empty"))
	}
	if utf8.RuneCountInString(displayName) > 200 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("display_name too long"))
	}
	expectedVersion := expectedVersion(msg.Version)
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetIdentity(ctx, ttx.Tx, tenantID, msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	row, err := db.UpdateIdentityDisplayName(ctx, ttx.Tx, tenantID, msg.Id, displayName, expectedVersion)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := audit.Record(ctx, ttx.Tx, audit.Entry{
		TenantID:        tenantID,
		ActorIdentityID: actor.ActorIdentityID,
		ActorType:       actor.ActorType,
		AuthMethod:      actor.AuthMethod,
		Action:          "identity.updated",
		TargetType:      "identity",
		TargetID:        row.ID,
		Before:          audit.Snapshot(identityAuditSnapshot(current)),
		After:           audit.Snapshot(identityAuditSnapshot(row)),
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit identity.updated: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("identity updated", "id", row.ID, "tenant", tenantID)
	return connect.NewResponse(&apiv1.UpdateIdentityResponse{Identity: identityRowToProto(row)}), nil
}

// SetIdentityStatus flips an identity between "active" and "disabled".
// Only those two values are writable; the column is unconstrained text
// so the handler validates the enum even though the schema does not. A
// version supplied on the request must match the current row or the
// update fails with NotFound.
func (s *Service) SetIdentityStatus(ctx context.Context, req *connect.Request[apiv1.SetIdentityStatusRequest]) (*connect.Response[apiv1.SetIdentityStatusResponse], error) {
	msg := req.Msg
	actor := ActorFromContext(ctx)
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	if msg.Status != "active" && msg.Status != "disabled" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("status must be \"active\" or \"disabled\""))
	}
	expectedVersion := expectedVersion(msg.Version)
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetIdentity(ctx, ttx.Tx, tenantID, msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	row, err := db.SetIdentityStatus(ctx, ttx.Tx, tenantID, msg.Id, msg.Status, expectedVersion)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := audit.Record(ctx, ttx.Tx, audit.Entry{
		TenantID:        tenantID,
		ActorIdentityID: actor.ActorIdentityID,
		ActorType:       actor.ActorType,
		AuthMethod:      actor.AuthMethod,
		Action:          "identity.status_changed",
		TargetType:      "identity",
		TargetID:        row.ID,
		Before:          audit.SnapshotStatus(current.Status),
		After:           audit.SnapshotStatus(row.Status),
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit identity.status_changed: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("identity status set", "id", row.ID, "status", row.Status, "tenant", tenantID)
	return connect.NewResponse(&apiv1.SetIdentityStatusResponse{Identity: identityRowToProto(row)}), nil
}

// DeleteIdentity hard-deletes an identity plus its role bindings, API
// keys, and local credentials in one tenant-scoped transaction. Two
// guards protect the admin surface: the caller may not delete their own
// identity (self-lockout), and an "active" identity must be disabled
// first (two-phase delete, matching the AC's disable-then-delete flow and
// preventing accidental mass deletion).
func (s *Service) DeleteIdentity(ctx context.Context, req *connect.Request[apiv1.DeleteIdentityRequest]) (*connect.Response[apiv1.DeleteIdentityResponse], error) {
	msg := req.Msg
	actor := ActorFromContext(ctx)
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	// Guard A: never let the caller delete the identity they are
	// authenticated as (would strand the session mid-request and can lock
	// an admin out of the plane).
	if ident, ok := FromContext(ctx); ok && ident.IdentityID == msg.Id {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cannot delete your own identity"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetIdentity(ctx, ttx.Tx, tenantID, msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	// Guard B: two-phase delete — an active identity must be disabled
	// before it can be deleted.
	if current.Status != "disabled" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("identity must be disabled before it can be deleted"))
	}
	if err := db.DeleteIdentity(ctx, ttx.Tx, tenantID, msg.Id); err != nil {
		return nil, mapDBError(err)
	}
	if err := audit.Record(ctx, ttx.Tx, audit.Entry{
		TenantID:        tenantID,
		ActorIdentityID: actor.ActorIdentityID,
		ActorType:       actor.ActorType,
		AuthMethod:      actor.AuthMethod,
		Action:          "identity.deleted",
		TargetType:      "identity",
		TargetID:        msg.Id,
		Before:          audit.Snapshot(identityAuditSnapshot(current)),
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit identity.deleted: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("identity deleted", "id", msg.Id, "tenant", tenantID)
	return connect.NewResponse(&apiv1.DeleteIdentityResponse{}), nil
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
	actor := ActorFromContext(ctx)
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
	if err := audit.Record(ctx, ttx.Tx, audit.Entry{
		TenantID:        tenantID,
		ActorIdentityID: actor.ActorIdentityID,
		ActorType:       actor.ActorType,
		AuthMethod:      actor.AuthMethod,
		Action:          "role.created",
		TargetType:      "role",
		TargetID:        row.ID,
		After:           audit.Snapshot(roleAuditSnapshot(row)),
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit role.created: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	return connect.NewResponse(&apiv1.CreateRoleResponse{Role: roleRowToProto(row)}), nil
}

// UpdateRole edits a role's name and/or entitlements. scope/scope_ref
// are immutable in this UI (project-scoped bindings are a non-goal). The
// role named "admin" cannot be edited at all: it carries entitlements
// ["*"] and ListIdentityEntitlements treats a bound role named exactly
// "admin" as the tenant-admin bypass, so renaming it would strand the
// plane and re-entitling it could weaken the bypass.
func (s *Service) UpdateRole(ctx context.Context, req *connect.Request[apiv1.UpdateRoleRequest]) (*connect.Response[apiv1.UpdateRoleResponse], error) {
	msg := req.Msg
	actor := ActorFromContext(ctx)
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	if msg.Name == nil && msg.Entitlements == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("nothing to update"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetRole(ctx, ttx.Tx, tenantID, msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	if current.Name == "admin" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("the admin role cannot be edited"))
	}
	name := current.Name
	if msg.Name != nil {
		name = strings.TrimSpace(*msg.Name)
		if !roleNameRE.MatchString(name) {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("name must match %s", roleNameRE.String()))
		}
	}
	ents := current.Entitlements
	if msg.Entitlements != nil {
		ents, err = validateEntitlements(msg.Entitlements.Values)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
	}
	expectedVersion := expectedVersion(msg.Version)
	row, err := db.UpdateRole(ctx, ttx.Tx, tenantID, msg.Id, name, ents, expectedVersion)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := audit.Record(ctx, ttx.Tx, audit.Entry{
		TenantID:        tenantID,
		ActorIdentityID: actor.ActorIdentityID,
		ActorType:       actor.ActorType,
		AuthMethod:      actor.AuthMethod,
		Action:          "role.updated",
		TargetType:      "role",
		TargetID:        row.ID,
		Before:          audit.Snapshot(roleAuditSnapshot(current)),
		After:           audit.Snapshot(roleAuditSnapshot(row)),
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit role.updated: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("role updated", "id", row.ID, "tenant", tenantID)
	return connect.NewResponse(&apiv1.UpdateRoleResponse{Role: roleRowToProto(row)}), nil
}

// DeleteRole hard-deletes a role and its role bindings. The role named
// "admin" cannot be deleted (it is the tenant-admin carrier and deleting
// it would strand the plane without an admin).
func (s *Service) DeleteRole(ctx context.Context, req *connect.Request[apiv1.DeleteRoleRequest]) (*connect.Response[apiv1.DeleteRoleResponse], error) {
	msg := req.Msg
	actor := ActorFromContext(ctx)
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetRole(ctx, ttx.Tx, tenantID, msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	if current.Name == "admin" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("the admin role cannot be deleted"))
	}
	if err := db.DeleteRoleBindingsByRole(ctx, ttx.Tx, tenantID, msg.Id); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := db.DeleteRole(ctx, ttx.Tx, tenantID, msg.Id); err != nil {
		return nil, mapDBError(err)
	}
	if err := audit.Record(ctx, ttx.Tx, audit.Entry{
		TenantID:        tenantID,
		ActorIdentityID: actor.ActorIdentityID,
		ActorType:       actor.ActorType,
		AuthMethod:      actor.AuthMethod,
		Action:          "role.deleted",
		TargetType:      "role",
		TargetID:        msg.Id,
		Before:          audit.Snapshot(roleAuditSnapshot(current)),
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit role.deleted: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("role deleted", "id", msg.Id, "tenant", tenantID)
	return connect.NewResponse(&apiv1.DeleteRoleResponse{}), nil
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
	actor := ActorFromContext(ctx)
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
	if err := audit.Record(ctx, ttx.Tx, audit.Entry{
		TenantID:        tenantID,
		ActorIdentityID: actor.ActorIdentityID,
		ActorType:       actor.ActorType,
		AuthMethod:      actor.AuthMethod,
		Action:          "role_binding.assigned",
		TargetType:      "role_binding",
		TargetID:        binding.ID,
		After:           audit.Snapshot(bindingAuditSnapshot(binding)),
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit role_binding.assigned: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	return connect.NewResponse(&apiv1.AssignRoleResponse{Binding: bindingRowToProto(binding)}), nil
}

// RevokeRole removes a role binding.
func (s *Service) RevokeRole(ctx context.Context, req *connect.Request[apiv1.RevokeRoleRequest]) (*connect.Response[apiv1.RevokeRoleResponse], error) {
	actor := ActorFromContext(ctx)
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
	current, err := db.GetRoleBinding(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := db.DeleteRoleBinding(ctx, ttx.Tx, tenantID, req.Msg.Id); err != nil {
		return nil, mapDBError(err)
	}
	if err := audit.Record(ctx, ttx.Tx, audit.Entry{
		TenantID:        tenantID,
		ActorIdentityID: actor.ActorIdentityID,
		ActorType:       actor.ActorType,
		AuthMethod:      actor.AuthMethod,
		Action:          "role_binding.revoked",
		TargetType:      "role_binding",
		TargetID:        req.Msg.Id,
		Before:          audit.Snapshot(bindingAuditSnapshot(current)),
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit role_binding.revoked: %w", err))
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
	actor := ActorFromContext(ctx)
	if actor.TenantID == "" {
		return nil, connect.NewError(connect.CodeInternal, errors.New("no tenant in context"))
	}
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
	// The tenants table has no tenant_id column and no RLS, so the audit
	// row is scoped to the ACTOR's tenant (the admin's own tenant). Begin
	// the tx in the actor's tenant so the audit row + tenant insert commit
	// atomically.
	ttx, err := s.pool.BeginTenantTx(ctx, actor.TenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	row, err := db.CreateTenant(ctx, ttx.Tx, slug, name, budget)
	if err != nil {
		// Most likely a unique-constraint violation on slug.
		if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "unique constraint") {
			return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("tenant slug %q already in use", slug))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := audit.Record(ctx, ttx.Tx, audit.Entry{
		TenantID:        actor.TenantID,
		ActorIdentityID: actor.ActorIdentityID,
		ActorType:       actor.ActorType,
		AuthMethod:      actor.AuthMethod,
		Action:          "tenant.created",
		TargetType:      "tenant",
		TargetID:        row.ID,
		After:           audit.Snapshot(tenantAuditSnapshot(row)),
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit tenant.created: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("tenant created", "id", row.ID, "slug", row.Slug, "name", row.Name)
	return connect.NewResponse(&apiv1.CreateTenantResponse{Tenant: tenantRowToProto(row)}), nil
}

// tenantAuditSnapshot is the non-secret projection of a tenant row for
// the audit trail.
func tenantAuditSnapshot(r db.TenantRow) map[string]any {
	return map[string]any{
		"id":     r.ID,
		"slug":   r.Slug,
		"name":   r.Name,
		"status": r.Status,
	}
}

// --- Local-account credentials (embedded IdP boundary) ---------------------

// localUsernameRE bounds the login handle charset. Lowercase alphanumerics
// plus the separators common to email-style handles (. _ @ + -); the
// display surface mirrors the slug convention so login identifiers stay
// predictable.
var localUsernameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._@+-]*$`)

// SetLocalCredential creates or updates the local-account credential bound
// to an identity (upsert by tenant + identity). It is the admin write path
// for the embedded IdP's local accounts and stays inside the identity-
// provider boundary: the plaintext password is hashed with argon2id here and
// the stored hash is never returned. The RBAC interceptor gates this to
// auth:write (admin). No control-plane service or Ask Orchicon tool has a
// credential write path.
func (s *Service) SetLocalCredential(ctx context.Context, req *connect.Request[apiv1.SetLocalCredentialRequest]) (*connect.Response[apiv1.SetLocalCredentialResponse], error) {
	msg := req.Msg
	actor := ActorFromContext(ctx)
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if msg.IdentityId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("identity_id must not be empty"))
	}
	username := strings.TrimSpace(msg.Username)
	if username == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("username must not be empty"))
	}
	if len(username) > 255 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("username too long"))
	}
	if !localUsernameRE.MatchString(username) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("username must match ^[a-z0-9][a-z0-9._@+-]*$"))
	}
	if msg.Password == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("password must not be empty"))
	}
	if len(msg.Password) > MaxPasswordLen {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("password too long"))
	}
	hash, err := HashPassword(msg.Password)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	// The identity must exist; the FK would reject it anyway, this gives a
	// clean NotFound instead of a raw constraint error.
	if _, err := db.GetIdentity(ctx, ttx.Tx, tenantID, msg.IdentityId); err != nil {
		return nil, mapDBError(err)
	}
	// forcePasswordChange=false: setting a credential is an explicit choice
	// (admin action or the first-login forced change), so the flag clears.
	row, err := db.UpsertLocalCredential(ctx, ttx.Tx, tenantID, msg.IdentityId, username, hash, false)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "unique constraint") {
			return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("username already in use by another identity"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := audit.Record(ctx, ttx.Tx, audit.Entry{
		TenantID:        tenantID,
		ActorIdentityID: actor.ActorIdentityID,
		ActorType:       actor.ActorType,
		AuthMethod:      actor.AuthMethod,
		Action:          "identity.credential_set",
		TargetType:      "identity",
		TargetID:        msg.IdentityId,
		After:           audit.Snapshot(map[string]any{"username": row.Username, "status": row.Status}),
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit identity.credential_set: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("local credential set", "identity", msg.IdentityId, "tenant", tenantID)
	return connect.NewResponse(&apiv1.SetLocalCredentialResponse{
		Status:   row.Status,
		Username: row.Username,
	}), nil
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

// ListAuditEvents returns a page of audit_events rows (the actor-based
// trail written by internal/audit.Record). Distinct from ListAuditEntries
// (policy decisions): this is "who did what", keyset-paginated on
// (occurred_at, id), newest first. All filters are optional; start_time is
// an inclusive lower bound, end_time an exclusive upper bound on
// occurred_at (absent = unbounded, the 'epoch' sentinel convention).
func (s *Service) ListAuditEvents(ctx context.Context, req *connect.Request[apiv1.ListAuditEventsRequest]) (*connect.Response[apiv1.ListAuditEventsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	pageSize := int(req.Msg.PageSize)
	if pageSize <= 0 || pageSize > 1000 {
		pageSize = 100
	}
	rows, err := s.pool.ListAuditEvents(ctx, tenantID,
		req.Msg.Action, req.Msg.ActorId, req.Msg.TargetType, req.Msg.TargetId,
		req.Msg.PageToken, pageSize,
		auditStartTime(req.Msg.StartTime), auditEndTime(req.Msg.EndTime))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	resp := &apiv1.ListAuditEventsResponse{}
	for _, r := range rows {
		resp.Events = append(resp.Events, &apiv1.AuditEvent{
			Id:              r.ID,
			TenantId:        r.TenantID,
			ActorIdentityId: r.ActorIdentityID,
			ActorType:       r.ActorType,
			AuthMethod:      r.AuthMethod,
			Action:          r.Action,
			TargetType:      r.TargetType,
			TargetId:        r.TargetID,
			Before:          string(r.Before),
			After:           string(r.After),
			TraceId:         r.TraceID,
			OccurredAt:      timestamppb.New(r.OccurredAt),
		})
	}
	if len(rows) > 0 {
		resp.NextPageToken = rows[len(rows)-1].ID
	}
	return connect.NewResponse(resp), nil
}

// --- helpers ---------------------------------------------------------------

func requireTenant(ctx context.Context) (string, error) {
	id := tenant.FromContext(ctx)
	if id == "" {
		return "", errors.New("no tenant in context")
	}
	return id, nil
}

// auditStartTime converts the optional lower time bound to a time.Time;
// nil → zero time, which the data-access layer treats as unbounded via
// the 'epoch' sentinel (same convention as usage.go's StartTime).
func auditStartTime(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime()
}

// auditEndTime converts the optional exclusive upper time bound. Nil →
// zero time (unbounded); a present timestamp is passed through as-is.
func auditEndTime(ts *timestamppb.Timestamp) time.Time {
	return auditStartTime(ts)
}

// expectedVersion converts the optional version field on an identity
// mutation request into the data-access sentinel: nil (caller supplied
// no version) → -1 disables the optimistic-concurrency check; a present
// value must match the current row version or the update is a no-op
// (NotFound). The proto field is int32; the data-access layer uses int.
func expectedVersion(v *int32) int {
	if v == nil {
		return -1
	}
	return int(*v)
}

func mapDBError(err error) error {
	if errors.Is(err, db.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, errors.New("not found"))
	}
	return connect.NewError(connect.CodeInternal, err)
}

var roleNameRE = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// validEntitlementRE matches a well-formed "resource:action" entitlement.
// Wildcards are permitted (and consumed by rbac.Has): "*" (tenant admin),
// "*:action", "resource:*", and "resource:action". Anything else is
// rejected so dead strings like "project:create" cannot persist.
var validEntitlementRE = regexp.MustCompile(`^(?:\*|[\w-]+):(?:\*|[\w.-]+)$`)

// validateEntitlements normalizes + bounds-checks a list of
// "resource:action" entitlements. Only `*`, `*:action`, `resource:*`, and
// `resource:action` forms are accepted (the exact set the RBAC interceptor
// enforces); any malformed string is rejected with InvalidArgument so the
// picker cannot persist dead entitlements.
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
		if e != "*" && !validEntitlementRE.MatchString(e) {
			return nil, fmt.Errorf("entitlement must be resource:action or *, got %q", e)
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
		Username:     r.Username,
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

// --- Audit snapshot helpers -------------------------------------------------
//
// These are the non-secret projections of db rows for audit before/after
// JSON. D7 (architecture-notes): never snapshot password hashes, API-key
// hashes/prefixes, tokens, or HMAC secrets. The ApiKeyRow carries
// KeyHash + KeyPrefix — both are excluded here.

// identityAuditSnapshot excludes nothing secret (identity rows hold no
// credentials) but keeps the trail compact.
func identityAuditSnapshot(r db.IdentityRow) map[string]any {
	return map[string]any{
		"id":            r.ID,
		"subject":       r.Subject,
		"display_name":  r.DisplayName,
		"identity_type": r.IdentityType,
		"status":        r.Status,
		"version":       r.Version,
	}
}

func roleAuditSnapshot(r db.RoleRow) map[string]any {
	return map[string]any{
		"id":           r.ID,
		"name":         r.Name,
		"scope":        r.Scope,
		"scope_ref":    r.ScopeRef,
		"entitlements": r.Entitlements,
		"version":      r.Version,
	}
}

func bindingAuditSnapshot(r db.RoleBindingRow) map[string]any {
	return map[string]any{
		"id":          r.ID,
		"identity_id": r.IdentityID,
		"role_id":     r.RoleID,
		"scope":       r.Scope,
		"scope_ref":   r.ScopeRef,
	}
}

// apiKeyAuditSnapshot NEVER includes KeyHash or KeyPrefix (the only
// fields an auditor could use to reconstruct or brute-force the key).
func apiKeyAuditSnapshot(r db.ApiKeyRow) map[string]any {
	return map[string]any{
		"id":          r.ID,
		"identity_id": r.IdentityID,
		"name":        r.Name,
		"scopes":      r.Scopes,
		"status":      r.Status,
		"version":     r.Version,
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
