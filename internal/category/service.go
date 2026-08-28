package category

import (
	"context"
	"encoding/json"
	"strings"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/audit"
	"github.com/beardedparrott/orchicon/internal/auth"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"google.golang.org/protobuf/types/known/timestamppb"
	"github.com/jackc/pgx/v5"
	"log/slog"
)

type Service struct {
	pool *db.Pool
	log  *slog.Logger
	apiv1connect.UnimplementedCategoryServiceHandler
}

var _ apiv1connect.CategoryServiceHandler = (*Service)(nil)

func New(pool *db.Pool, log *slog.Logger) *Service { return &Service{pool: pool, log: log} }

func requireTenant(ctx context.Context) (string, error) {
	id := tenant.FromContext(ctx)
	if id == "" { return "", connect.NewError(connect.CodeUnauthenticated, nil) }
	return id, nil
}

func toProto(c db.CategoryRow) *apiv1.Category {
	return &apiv1.Category{
		Id: c.ID, TenantId: c.TenantID, TargetType: toProtoTarget(c.TargetType),
		Name: c.Name, Description: c.Description, Slug: c.Slug,
		SortOrder: int32(c.SortOrder), CreatedAt: timestamppb.New(c.CreatedAt), UpdatedAt: timestamppb.New(c.UpdatedAt),
	}
}
func toProtoTarget(s string) apiv1.CategoryTargetType {
	switch s {
	case "worker": return apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER
	case "workflow": return apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKFLOW
	case "conversation": return apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION
	default: return apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_UNSPECIFIED
	}
}
func fromProtoTarget(t apiv1.CategoryTargetType) string {
	switch t {
	case apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER: return "worker"
	case apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKFLOW: return "workflow"
	case apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION: return "conversation"
	default: return ""
	}
}

func (s *Service) ListCategories(ctx context.Context, req *connect.Request[apiv1.ListCategoriesRequest]) (*connect.Response[apiv1.ListCategoriesResponse], error) {
	tenantID, err := requireTenant(ctx); if err != nil { return nil, err }
	tt := fromProtoTarget(req.Msg.TargetType)
	if tt == "" { return nil, connect.NewError(connect.CodeInvalidArgument, nil) }
	tx, err := s.pool.BeginTenantTx(ctx, tenantID); if err != nil { return nil, connect.NewError(connect.CodeInternal, err) }
	defer tx.Rollback(ctx)
	cats, err := db.ListCategories(ctx, tx.Tx, tenantID, tt); if err != nil { return nil, connect.NewError(connect.CodeInternal, err) }
	assigns, err := db.ListAssignments(ctx, tx.Tx, tenantID, tt); if err != nil { return nil, connect.NewError(connect.CodeInternal, err) }
	_ = tx.Commit(ctx)
	var pc []*apiv1.Category
	for _, c := range cats { pc = append(pc, toProto(c)) }
	var pa []*apiv1.CategoryAssignment
	for _, a := range assigns { pa = append(pa, &apiv1.CategoryAssignment{EntityId: a.EntityID, CategoryId: a.CategoryID, TargetType: toProtoTarget(a.TargetType)}) }
	return connect.NewResponse(&apiv1.ListCategoriesResponse{Categories: pc, Assignments: pa}), nil
}

func (s *Service) CreateCategory(ctx context.Context, req *connect.Request[apiv1.CreateCategoryRequest]) (*connect.Response[apiv1.CreateCategoryResponse], error) {
	tenantID, err := requireTenant(ctx); if err != nil { return nil, err }
	tt := fromProtoTarget(req.Msg.TargetType)
	if tt == "" { return nil, connect.NewError(connect.CodeInvalidArgument, nil) }
	name := strings.TrimSpace(req.Msg.Name)
	if name == "" || len(name) > 64 { return nil, connect.NewError(connect.CodeInvalidArgument, nil) }
	if strings.EqualFold(name, "uncategorized") { return nil, connect.NewError(connect.CodeInvalidArgument, nil) }
	desc := strings.TrimSpace(req.Msg.Description)
	if len(desc) > 256 { return nil, connect.NewError(connect.CodeInvalidArgument, nil) }
	tx, err := s.pool.BeginTenantTx(ctx, tenantID); if err != nil { return nil, connect.NewError(connect.CodeInternal, err) }
	defer tx.Rollback(ctx)
	cat, err := db.CreateCategory(ctx, tx.Tx, tenantID, tt, name, desc)
	if err != nil {
		if strings.Contains(err.Error(), "already exists") { return nil, connect.NewError(connect.CodeAlreadyExists, err) }
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := recordAudit(ctx, tx.Tx, tenantID, "category.created", "category", cat.ID, nil, audit.Snapshot(cat)); err != nil { return nil, connect.NewError(connect.CodeInternal, err) }
	if err := tx.Commit(ctx); err != nil { return nil, connect.NewError(connect.CodeInternal, err) }
	return connect.NewResponse(&apiv1.CreateCategoryResponse{Category: toProto(cat)}), nil
}

func (s *Service) UpdateCategory(ctx context.Context, req *connect.Request[apiv1.UpdateCategoryRequest]) (*connect.Response[apiv1.UpdateCategoryResponse], error) {
	tenantID, err := requireTenant(ctx); if err != nil { return nil, err }
	if req.Msg.Id == "" { return nil, connect.NewError(connect.CodeInvalidArgument, nil) }
	var namePtr *string
	if req.Msg.Name != "" { n := strings.TrimSpace(req.Msg.Name); namePtr = &n }
	var descPtr *string
	if req.Msg.Description != "" { d := strings.TrimSpace(req.Msg.Description); descPtr = &d }
	// If both empty, no-op (but still require at least one)
	if namePtr == nil && descPtr == nil { return nil, connect.NewError(connect.CodeInvalidArgument, nil) }
	tx, err := s.pool.BeginTenantTx(ctx, tenantID); if err != nil { return nil, connect.NewError(connect.CodeInternal, err) }
	defer tx.Rollback(ctx)
	before, err := db.GetCategory(ctx, tx.Tx, tenantID, req.Msg.Id); if err != nil { return nil, connect.NewError(connect.CodeNotFound, err) }
	updated, err := db.UpdateCategory(ctx, tx.Tx, tenantID, req.Msg.Id, namePtr, descPtr)
	if err != nil {
		if strings.Contains(err.Error(), "already exists") { return nil, connect.NewError(connect.CodeAlreadyExists, err) }
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := recordAudit(ctx, tx.Tx, tenantID, "category.updated", "category", updated.ID, audit.Snapshot(before), audit.Snapshot(updated)); err != nil { return nil, connect.NewError(connect.CodeInternal, err) }
	if err := tx.Commit(ctx); err != nil { return nil, connect.NewError(connect.CodeInternal, err) }
	return connect.NewResponse(&apiv1.UpdateCategoryResponse{Category: toProto(updated)}), nil
}

func (s *Service) DeleteCategory(ctx context.Context, req *connect.Request[apiv1.DeleteCategoryRequest]) (*connect.Response[apiv1.DeleteCategoryResponse], error) {
	tenantID, err := requireTenant(ctx); if err != nil { return nil, err }
	if req.Msg.Id == "" { return nil, connect.NewError(connect.CodeInvalidArgument, nil) }
	tx, err := s.pool.BeginTenantTx(ctx, tenantID); if err != nil { return nil, connect.NewError(connect.CodeInternal, err) }
	defer tx.Rollback(ctx)
	before, err := db.GetCategory(ctx, tx.Tx, tenantID, req.Msg.Id); if err != nil { return nil, connect.NewError(connect.CodeNotFound, err) }
	if err := db.DeleteCategory(ctx, tx.Tx, tenantID, req.Msg.Id); err != nil { return nil, connect.NewError(connect.CodeInternal, err) }
	if err := recordAudit(ctx, tx.Tx, tenantID, "category.deleted", "category", before.ID, audit.Snapshot(before), nil); err != nil { return nil, connect.NewError(connect.CodeInternal, err) }
	if err := tx.Commit(ctx); err != nil { return nil, connect.NewError(connect.CodeInternal, err) }
	return connect.NewResponse(&apiv1.DeleteCategoryResponse{}), nil
}

func (s *Service) AssignToCategory(ctx context.Context, req *connect.Request[apiv1.AssignToCategoryRequest]) (*connect.Response[apiv1.AssignToCategoryResponse], error) {
	tenantID, err := requireTenant(ctx); if err != nil { return nil, err }
	tt := fromProtoTarget(req.Msg.TargetType)
	if tt == "" { return nil, connect.NewError(connect.CodeInvalidArgument, nil) }
	if req.Msg.EntityId == "" || req.Msg.CategoryId == "" { return nil, connect.NewError(connect.CodeInvalidArgument, nil) }
	tx, err := s.pool.BeginTenantTx(ctx, tenantID); if err != nil { return nil, connect.NewError(connect.CodeInternal, err) }
	defer tx.Rollback(ctx)
	if err := db.AssignToCategory(ctx, tx.Tx, tenantID, tt, req.Msg.EntityId, req.Msg.CategoryId); err != nil {
		if strings.Contains(err.Error(), "mismatch") { return nil, connect.NewError(connect.CodeInvalidArgument, err) }
		if err == db.ErrNotFound { return nil, connect.NewError(connect.CodeNotFound, err) }
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := recordAudit(ctx, tx.Tx, tenantID, "category.assigned", "category", req.Msg.CategoryId, nil, audit.Snapshot(map[string]string{"entity_id": req.Msg.EntityId, "target_type": tt})); err != nil { return nil, connect.NewError(connect.CodeInternal, err) }
	if err := tx.Commit(ctx); err != nil { return nil, connect.NewError(connect.CodeInternal, err) }
	return connect.NewResponse(&apiv1.AssignToCategoryResponse{}), nil
}

func (s *Service) UnassignFromCategory(ctx context.Context, req *connect.Request[apiv1.UnassignFromCategoryRequest]) (*connect.Response[apiv1.UnassignFromCategoryResponse], error) {
	tenantID, err := requireTenant(ctx); if err != nil { return nil, err }
	tt := fromProtoTarget(req.Msg.TargetType)
	if tt == "" { return nil, connect.NewError(connect.CodeInvalidArgument, nil) }
	if req.Msg.EntityId == "" { return nil, connect.NewError(connect.CodeInvalidArgument, nil) }
	tx, err := s.pool.BeginTenantTx(ctx, tenantID); if err != nil { return nil, connect.NewError(connect.CodeInternal, err) }
	defer tx.Rollback(ctx)
	_ = db.UnassignFromCategory(ctx, tx.Tx, tenantID, tt, req.Msg.EntityId)
	if err := recordAudit(ctx, tx.Tx, tenantID, "category.unassigned", "category", req.Msg.EntityId, nil, nil); err != nil { return nil, connect.NewError(connect.CodeInternal, err) }
	if err := tx.Commit(ctx); err != nil { return nil, connect.NewError(connect.CodeInternal, err) }
	return connect.NewResponse(&apiv1.UnassignFromCategoryResponse{}), nil
}

func (s *Service) ReorderCategories(ctx context.Context, req *connect.Request[apiv1.ReorderCategoriesRequest]) (*connect.Response[apiv1.ReorderCategoriesResponse], error) {
	tenantID, err := requireTenant(ctx); if err != nil { return nil, err }
	tt := fromProtoTarget(req.Msg.TargetType)
	if tt == "" { return nil, connect.NewError(connect.CodeInvalidArgument, nil) }
	if len(req.Msg.OrderedIds) == 0 { return nil, connect.NewError(connect.CodeInvalidArgument, nil) }
	tx, err := s.pool.BeginTenantTx(ctx, tenantID); if err != nil { return nil, connect.NewError(connect.CodeInternal, err) }
	defer tx.Rollback(ctx)
	before, _ := db.ListCategories(ctx, tx.Tx, tenantID, tt)
	if err := db.ReorderCategories(ctx, tx.Tx, tenantID, tt, req.Msg.OrderedIds); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	after, _ := db.ListCategories(ctx, tx.Tx, tenantID, tt)
	if err := recordAudit(ctx, tx.Tx, tenantID, "category.reordered", "category", tenantID, audit.Snapshot(before), audit.Snapshot(after)); err != nil { return nil, connect.NewError(connect.CodeInternal, err) }
	if err := tx.Commit(ctx); err != nil { return nil, connect.NewError(connect.CodeInternal, err) }
	return connect.NewResponse(&apiv1.ReorderCategoriesResponse{}), nil
}

func recordAudit(ctx context.Context, tx pgx.Tx, tenantID, action, targetType, targetID string, before, after json.RawMessage) error {
	e := auth.ActorFromContext(ctx)
	if e.TenantID == "" {
		e.TenantID = tenantID
	}
	e.Action = action
	e.TargetType = targetType
	e.TargetID = targetID
	e.Before = before
	e.After = after
	return audit.Record(ctx, tx, e)
}
