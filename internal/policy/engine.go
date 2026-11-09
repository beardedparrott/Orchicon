// Package policy implements the Rego-based Policy Engine (docs/02_Domain_Model.md §2.5 Tier 1,
// docs/07 §3.5).
package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/open-policy-agent/opa/v1/rego"
	"github.com/open-policy-agent/opa/v1/topdown"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/jackc/pgx/v5"
)

// Engine is the Rego-based Policy Engine.
type Engine struct {
	pool *db.Pool
	log  *slog.Logger
}

// New constructs a Policy Engine.
func New(pool *db.Pool, log *slog.Logger) *Engine {
	return &Engine{pool: pool, log: log}
}

// Decision is the result of a Policy evaluation.
type Decision struct {
	Effect        string
	PolicyID      string
	PolicyVersion int
	Scope         string
	ScopeRef      string
	Result        []byte
	Trace         []byte
	Err           string
}

// Evaluate evaluates all published policies at a decision point for the given tenant, in
// narrowest-scope-first order, and returns the first definitive decision (docs/02 §2.5).
func (e *Engine) Evaluate(ctx context.Context, tenantID, decisionPoint, targetType, targetID string, input any) (Decision, error) {
	ttx, err := e.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return Decision{Effect: domain.PolicyEffectAllow}, fmt.Errorf("policy: begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)

	candidates, err := db.ListPublishedPoliciesByDecisionPoint(ctx, ttx.Tx, tenantID, decisionPoint)
	if err != nil {
		return Decision{Effect: domain.PolicyEffectAllow}, fmt.Errorf("policy: list published: %w", err)
	}
	if len(candidates) == 0 {
		// No published policies at this decision point → governance floor
		// (docs/02 §2.5: "basic by default, always-on"). Allow.
		return Decision{Effect: domain.PolicyEffectAllow}, nil
	}

	inputJSON, _ := json.Marshal(input)
	dec := Decision{Effect: domain.PolicyEffectAllow, Result: []byte("{}"), Trace: []byte("[]")}
	for _, pv := range candidates {
		result, trace, allowed, evalErr := e.evalModule(ctx, pv, input)
		if evalErr != nil {
			// Record the error but continue (fail-open — docs/02 §2.5 governance floor).
			e.log.Warn("policy evaluation error",
				"policy", pv.PolicyID, "version", pv.Version,
				"decision_point", decisionPoint, "error", evalErr)
			dec.Err = evalErr.Error()
			continue
		}
		if !allowed {
			// This policy's rule did not match (query → false). Next.
			continue
		}
		// Definitive decision: this policy matched (query → true).
		dec = Decision{
			Effect:        pv.Effect,
			PolicyID:      pv.PolicyID,
			PolicyVersion: pv.Version,
			Scope:         pv.Scope,
			ScopeRef:      pv.ScopeRef,
			Result:        result,
			Trace:         trace,
		}
		_ = inputJSON
		return dec, nil
	}
	return dec, nil
}

// EvaluateVersion evaluates a single specific policy version (dry-run for the policy editor /
// EvaluatePolicy RPC with policy_id set).
func (e *Engine) EvaluateVersion(ctx context.Context, tenantID, policyID string, version int, input any) (Decision, error) {
	ttx, err := e.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return Decision{Effect: domain.PolicyEffectAllow}, fmt.Errorf("policy: begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)

	pv, err := db.GetPolicyVersion(ctx, ttx.Tx, tenantID, policyID, version)
	if err != nil {
		return Decision{Effect: domain.PolicyEffectAllow}, fmt.Errorf("policy: get version: %w", err)
	}
	result, trace, allowed, evalErr := e.evalModule(ctx, pv, input)
	dec := Decision{
		Effect:        domain.PolicyEffectAllow,
		PolicyID:      pv.PolicyID,
		PolicyVersion: pv.Version,
		Scope:         pv.Scope,
		ScopeRef:      pv.ScopeRef,
		Result:        result,
		Trace:         trace,
	}
	if evalErr != nil {
		dec.Err = evalErr.Error()
		return dec, nil
	}
	if allowed {
		dec.Effect = pv.Effect
	}
	return dec, nil
}

// EvaluateGate evaluates a gate_policy_ref at the dispatch decision point (docs/02 §2.5 Tier 1).
func (e *Engine) EvaluateGate(ctx context.Context, tenantID, gatePolicyRef, targetType, targetID string, input any) (bool, error) {
	dec, err := e.Evaluate(ctx, tenantID, domain.DecisionPointDispatch, targetType, targetID, input)
	if err != nil {
		// Fail-open: a policy engine error does not block the DAG (docs/02 §2.5 governance floor).
		return true, nil
	}
	switch dec.Effect {
	case domain.PolicyEffectAllow:
		return true, nil
	case domain.PolicyEffectDeny:
		return false, nil
	case domain.PolicyEffectRequireApproval, domain.PolicyEffectRequireReview:
		// Route to a human / Reviewer. For the gate, this blocks the
		// automatic transition (the step moves to blocked/approval).
		return false, nil
	}
	return true, nil
}

// evalModule compiles + evaluates a single Rego module against the input, capturing the evaluation
// trace.
func (e *Engine) evalModule(ctx context.Context, pv db.PolicyVersionRow, input any) (result []byte, trace []byte, allowed bool, err error) {
	query := pv.Query
	if query == "" {
		// Default query: the policy's `allow` rule (data.orchicon.policy.<policy_id>.allow).
		query = "data.orchicon.policy.allow"
	}
	buf := topdown.NewBufferTracer()
	r := rego.New(
		rego.Query(query),
		rego.Module(pv.PolicyID+".rego", pv.RegoModule),
		rego.QueryTracer(buf),
	)
	prepared, err := r.PrepareForEval(ctx)
	if err != nil {
		return []byte("{}"), []byte("[]"), false, fmt.Errorf("prepare: %w", err)
	}
	rs, evalErr := prepared.Eval(ctx, rego.EvalInput(input))
	if evalErr != nil {
		return []byte("{}"), []byte("[]"), false, fmt.Errorf("eval: %w", evalErr)
	}
	// Marshal the result set.
	resultJSON, _ := json.Marshal(rs)
	if resultJSON == nil {
		resultJSON = []byte("{}")
	}
	// Marshal the trace events.
	traceJSON, _ := json.Marshal([]*topdown.Event(*buf))
	if traceJSON == nil {
		traceJSON = []byte("[]")
	}
	return resultJSON, traceJSON, rs.Allowed(), nil
}

// RecordDecision persists a PolicyDecision row so ExplainDecision can return the Rego trace for a
// past policy.evaluated event (docs/07 §3.5).
func RecordDecision(ctx context.Context, tx pgx.Tx, d db.PolicyDecisionRow) error {
	if d.ID == "" {
		d.ID = db.NewID()
	}
	if d.OccurredAt.IsZero() {
		d.OccurredAt = time.Now().UTC()
	}
	_, err := db.CreatePolicyDecision(ctx, tx, d)
	if err != nil {
		return fmt.Errorf("policy: record decision: %w", err)
	}
	return nil
}

// EnqueuePolicyEvent enqueues a policy.evaluated outbox event so streaming clients and webhook
// subscribers see the decision (docs/08 §4.4).
func EnqueuePolicyEvent(ctx context.Context, tx pgx.Tx, eventType string, d db.PolicyDecisionRow) error {
	evt := map[string]any{
		"event_type":     eventType,
		"tenant_id":      d.TenantID,
		"decision_id":    d.ID,
		"policy_id":      d.PolicyID,
		"policy_version": d.PolicyVersion,
		"decision_point": d.DecisionPoint,
		"effect":         d.Effect,
		"target_type":    d.TargetType,
		"target_id":      d.TargetID,
		"actor_type":     d.ActorType,
		"actor_id":       d.ActorID,
		"trace_id":       d.TraceID,
		"occurred_at":    d.OccurredAt.Format(time.RFC3339Nano),
	}
	payload, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("policy: marshal event: %w", err)
	}
	return db.EnqueueOutbox(ctx, tx, db.OutboxRow{
		TenantID:      d.TenantID,
		EventType:     eventType,
		AggregateType: "policy",
		AggregateID:   d.ID,
		Payload:       payload,
		OccurredAt:    d.OccurredAt,
	})
}

// CompileModule validates a Rego module compiles (used by the publish path to reject malformed Rego
// before persisting).
func CompileModule(ctx context.Context, policyID, source, query string) error {
	if query == "" {
		query = "data.orchicon.policy.allow"
	}
	r := rego.New(
		rego.Query(query),
		rego.Module(policyID+".rego", source),
	)
	if _, err := r.PrepareForEval(ctx); err != nil {
		return fmt.Errorf("compile: %w", err)
	}
	return nil
}
