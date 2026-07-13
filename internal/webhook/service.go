package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	apiv1connect "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/auth"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/eventbus"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Service implements the WebhookService Connect handler
// (docs/07_API_Specification.md §3.11). Subscriptions are tenant-scoped;
// deliveries are recorded per attempt with retry + dead-letter.
type Service struct {
	pool        *db.Pool
	log         *slog.Logger
	dispatcher  *Dispatcher
	subscriber  eventbus.Subscriber
	apiv1connect.UnimplementedWebhookServiceHandler
}

// Compile-time assertion that Service satisfies the handler interface.
var _ apiv1connect.WebhookServiceHandler = (*Service)(nil)

// NewService constructs a WebhookService handler. The dispatcher may be
// nil (webhooks disabled when NATS is unavailable).
func NewService(pool *db.Pool, log *slog.Logger, dispatcher *Dispatcher, sub eventbus.Subscriber) *Service {
	return &Service{pool: pool, log: log, dispatcher: dispatcher, subscriber: sub}
}

// maxURLLen bounds the target_url to prevent abuse.
const maxURLLen = 2048

// CreateSubscription validates + creates a webhook subscription.
func (s *Service) CreateSubscription(ctx context.Context, req *connect.Request[apiv1.CreateSubscriptionRequest]) (*connect.Response[apiv1.CreateSubscriptionResponse], error) {
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
	targetURL, err := validateTargetURL(msg.TargetUrl)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	filter := strings.TrimSpace(msg.EventFilter)
	if filter == "" {
		filter = "*"
	}
	scope := msg.Scope
	if scope == "" {
		scope = "tenant"
	}
	if scope != "tenant" && scope != "project" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid scope %q", scope))
	}
	secret := strings.TrimSpace(msg.Secret)
	secretHint := ""
	secretHash := ""
	if secret != "" {
		secretHint = secret[:min(6, len(secret))]
		secretHash = hashSecret(secret)
	}
	maxRetries := int(msg.MaxRetries)
	if maxRetries <= 0 {
		maxRetries = 5
	}
	if maxRetries > 20 {
		maxRetries = 20
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	row, err := db.CreateSubscription(ctx, ttx.Tx, db.EventSubscriptionRow{
		TenantID:    tenantID,
		Name:        name,
		TargetURL:   targetURL,
		EventFilter: filter,
		Scope:       scope,
		ScopeRef:    msg.ScopeRef,
		SecretHint:  secretHint,
		SecretHash:  secretHash,
		MaxRetries:  maxRetries,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("webhook subscription created", "id", row.ID, "tenant", tenantID)
	return connect.NewResponse(&apiv1.CreateSubscriptionResponse{Subscription: subRowToProto(row)}), nil
}

// GetSubscription returns a single subscription by id.
func (s *Service) GetSubscription(ctx context.Context, req *connect.Request[apiv1.GetSubscriptionRequest]) (*connect.Response[apiv1.GetSubscriptionResponse], error) {
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
	row, err := db.GetSubscription(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	return connect.NewResponse(&apiv1.GetSubscriptionResponse{Subscription: subRowToProto(row)}), nil
}

// ListSubscriptions returns a page of subscriptions.
func (s *Service) ListSubscriptions(ctx context.Context, req *connect.Request[apiv1.ListSubscriptionsRequest]) (*connect.Response[apiv1.ListSubscriptionsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	rows, err := db.ListSubscriptions(ctx, ttx.Tx, tenantID, int(req.Msg.PageSize), req.Msg.PageToken)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	resp := &apiv1.ListSubscriptionsResponse{}
	for _, r := range rows {
		resp.Subscriptions = append(resp.Subscriptions, subRowToProto(r))
	}
	if len(rows) > 0 {
		resp.NextPageToken = rows[len(rows)-1].ID
	}
	return connect.NewResponse(resp), nil
}

// UpdateSubscription applies a partial update with optimistic concurrency.
func (s *Service) UpdateSubscription(ctx context.Context, req *connect.Request[apiv1.UpdateSubscriptionRequest]) (*connect.Response[apiv1.UpdateSubscriptionResponse], error) {
	msg := req.Msg
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	var fields db.UpdateSubscriptionFields
	if msg.TargetUrl != nil {
		u, err := validateTargetURL(*msg.TargetUrl)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		fields.TargetURL = &u
	}
	if msg.EventFilter != nil {
		f := strings.TrimSpace(*msg.EventFilter)
		if f == "" {
			f = "*"
		}
		fields.EventFilter = &f
	}
	if msg.Status != nil {
		st := strings.TrimSpace(*msg.Status)
		if st != "active" && st != "paused" {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("status must be active or paused"))
		}
		fields.Status = &st
	}
	if msg.MaxRetries != nil {
		mr := int(*msg.MaxRetries)
		if mr < 0 || mr > 20 {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("max_retries out of range"))
		}
		fields.MaxRetries = &mr
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetSubscription(ctx, ttx.Tx, tenantID, msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	updated, err := db.UpdateSubscription(ctx, ttx.Tx, tenantID, msg.Id, current.Version, fields)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	return connect.NewResponse(&apiv1.UpdateSubscriptionResponse{Subscription: subRowToProto(updated)}), nil
}

// DeleteSubscription soft-deletes a subscription.
func (s *Service) DeleteSubscription(ctx context.Context, req *connect.Request[apiv1.DeleteSubscriptionRequest]) (*connect.Response[apiv1.DeleteSubscriptionResponse], error) {
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
	current, err := db.GetSubscription(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := db.SoftDeleteSubscription(ctx, ttx.Tx, tenantID, req.Msg.Id, current.Version); err != nil {
		return nil, mapDBError(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	return connect.NewResponse(&apiv1.DeleteSubscriptionResponse{}), nil
}

// TestSubscription sends a test event to the subscription endpoint.
func (s *Service) TestSubscription(ctx context.Context, req *connect.Request[apiv1.TestSubscriptionRequest]) (*connect.Response[apiv1.TestSubscriptionResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	if s.dispatcher == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("webhook dispatcher unavailable"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	sub, err := db.GetSubscription(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	_ = ttx.Rollback(ctx)
	testData, _ := json.Marshal(map[string]any{
		"event_type": "webhook.test",
		"tenant_id":  tenantID,
		"message":   "Orchicon webhook test event",
	})
	delivery, err := db.CreateDelivery(ctx, s.pool, db.WebhookDeliveryRow{
		TenantID:       tenantID,
		SubscriptionID: sub.ID,
		EventID:        "test-" + db.NewID(),
		EventType:      "webhook.test",
		Payload:        testData,
		Status:         "retrying",
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	statusCode, derr := s.dispatcher.postOnce(ctx, sub, testData, delivery.ID)
	if derr == nil && statusCode >= 200 && statusCode < 300 {
		_ = db.UpdateDeliveryResult(ctx, s.pool, delivery.ID, statusCode, "delivered", "", nil)
	} else {
		errMsg := ""
		if derr != nil {
			errMsg = derr.Error()
		} else {
			errMsg = fmt.Sprintf("HTTP %d", statusCode)
		}
		_ = db.UpdateDeliveryResult(ctx, s.pool, delivery.ID, statusCode, "dead_letter", errMsg, nil)
	}
	updated, _ := db.GetDelivery(ctx, ttx.Tx, tenantID, delivery.ID)
	return connect.NewResponse(&apiv1.TestSubscriptionResponse{Delivery: deliveryRowToProto(updated)}), nil
}

// ListDeliveries returns a page of delivery attempts.
func (s *Service) ListDeliveries(ctx context.Context, req *connect.Request[apiv1.ListDeliveriesRequest]) (*connect.Response[apiv1.ListDeliveriesResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	rows, err := db.ListDeliveries(ctx, ttx.Tx, tenantID, req.Msg.SubscriptionId, req.Msg.Status,
		int(req.Msg.PageSize), req.Msg.PageToken)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	resp := &apiv1.ListDeliveriesResponse{}
	for _, r := range rows {
		resp.Deliveries = append(resp.Deliveries, deliveryRowToProto(r))
	}
	if len(rows) > 0 {
		resp.NextPageToken = rows[len(rows)-1].ID
	}
	return connect.NewResponse(resp), nil
}

// ReplayDelivery re-enqueues a dead-lettered delivery.
func (s *Service) ReplayDelivery(ctx context.Context, req *connect.Request[apiv1.ReplayDeliveryRequest]) (*connect.Response[apiv1.ReplayDeliveryResponse], error) {
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
	orig, err := db.GetDelivery(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	_ = ttx.Rollback(ctx)
	replayed, err := db.ReenqueueDelivery(ctx, s.pool, orig)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apiv1.ReplayDeliveryResponse{Delivery: deliveryRowToProto(replayed)}), nil
}

// StreamSubscriptionDeliveries streams delivery events for a subscription.
func (s *Service) StreamSubscriptionDeliveries(ctx context.Context, req *connect.Request[apiv1.StreamSubscriptionDeliveriesRequest], stream *connect.ServerStream[apiv1.StreamSubscriptionDeliveriesResponse]) error {
	if s.subscriber == nil {
		return connect.NewError(connect.CodeUnavailable, errors.New("event streaming unavailable"))
	}
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	var fromSeq uint64
	if req.Msg.FromSequence != nil && *req.Msg.FromSequence > 0 {
		fromSeq = uint64(*req.Msg.FromSequence)
	}
	ch, err := s.subscriber.Subscribe(ctx, "orchicon.events.>", fromSeq)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("subscribe: %w", err))
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-ch:
			if !ok {
				return nil
			}
			var env struct {
				EventType string `json:"event_type"`
				EventID   string `json:"aggregate_id"`
			}
			_ = json.Unmarshal(msg.Data, &env)
			delivery := &apiv1.WebhookDelivery{
				TenantId:    tenantID,
				EventId:     env.EventID,
				EventType:   env.EventType,
				Status:      "delivered",
				OccurredAt:  timestamppb.Now(),
			}
			if err := stream.Send(&apiv1.StreamSubscriptionDeliveriesResponse{
				Delivery: delivery, Sequence: int64(msg.Seq),
			}); err != nil {
				return err
			}
		}
	}
}

// --- helpers ---------------------------------------------------------------

func requireTenant(ctx context.Context) (string, error) {
	id := tenant.FromContext(ctx)
	if id == "" {
		// Fall back to the auth identity's tenant if present.
		if ident, ok := auth.FromContext(ctx); ok && ident.TenantID != "" {
			return ident.TenantID, nil
		}
		return "", errors.New("no tenant in context")
	}
	return id, nil
}

func validateTargetURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("target_url must not be empty")
	}
	if len(raw) > maxURLLen {
		return "", errors.New("target_url too long")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid target_url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("target_url must be http or https")
	}
	if u.Host == "" {
		return "", errors.New("target_url must have a host")
	}
	return raw, nil
}

func mapDBError(err error) error {
	if errors.Is(err, db.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, errors.New("not found"))
	}
	return connect.NewError(connect.CodeInternal, err)
}

func subRowToProto(r db.EventSubscriptionRow) *apiv1.WebhookSubscription {
	return &apiv1.WebhookSubscription{
		Id:          r.ID,
		TenantId:    r.TenantID,
		Name:        r.Name,
		TargetUrl:   r.TargetURL,
		EventFilter: r.EventFilter,
		Scope:       r.Scope,
		ScopeRef:    r.ScopeRef,
		SecretHint:  r.SecretHint,
		MaxRetries:  int32(r.MaxRetries),
		Status:      r.Status,
		Version:     int32(r.Version),
		CreatedAt:   timestamppb.New(r.CreatedAt),
		UpdatedAt:   timestamppb.New(r.UpdatedAt),
	}
}

func deliveryRowToProto(r db.WebhookDeliveryRow) *apiv1.WebhookDelivery {
	if r.ID == "" {
		return &apiv1.WebhookDelivery{}
	}
	d := &apiv1.WebhookDelivery{
		Id:             r.ID,
		TenantId:       r.TenantID,
		SubscriptionId: r.SubscriptionID,
		EventId:        r.EventID,
		EventType:      r.EventType,
		Attempt:        int32(r.Attempt),
		StatusCode:     int32(r.StatusCode),
		Status:         r.Status,
		Error:          r.Error,
		OccurredAt:     timestamppb.New(r.OccurredAt),
	}
	return d
}

func hashSecret(secret string) string {
	return auth.HashApiKey(secret)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// pgx import retained for future tenant-tx reads in streaming.
var _ = pgx.ErrNoRows
var _ = time.Now
