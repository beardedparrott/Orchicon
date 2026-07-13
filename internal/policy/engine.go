// Package policy implements the Rego-based Policy Engine
// (docs/02_Domain_Model.md §2.5 Tier 1, docs/07 §3.5).
//
// Tier 1 (decision-point) policy is the v0.1 baseline, always-on,
// Rego-only (Open Policy Agent). The engine evaluates published Policy
// versions at well-defined decision points (admission, dispatch, budget,
// approval, recovery, completion), applying narrowest-scope-match-wins
// then first-definitive-decision-wins (docs/02 §2.5).
//
// v0.1 loads policies on-demand from Postgres (the policies + their Rego
// modules live in the policy_versions table). A compiled-bundle mode is
// a v0.2 optimization; the on-demand path is correct and sufficient for
// v0.1 volume. Each evaluation produces a Rego evaluation trace persisted
// as a PolicyDecision so ExplainDecision can return it later.
//
// Go hooks (imperative escape hatch for policies needing live data) are
// deferred to v0.2 (docs/02 §2.5 Tier 1).
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

// Engine is the Rego-based Policy Engine. It is safe for concurrent use
// (each evaluation prepares its own query; OPA's rego package is
// goroutine-safe for PrepareForEval + Eval).
type Engine struct {
	pool *db.Pool
	log  *slog.Logger
}

// New constructs a Policy Engine.
func New(pool *db.Pool, log *slog.Logger) *Engine {
	return &Engine{pool: pool, log: log}
}

// Decision is the result of a Policy evaluation. Effect is the decisive
// effect (allow | deny | require_approval | require_review); PolicyID +
// PolicyVersion identify the policy that produced the decisive effect
// (empty when no published policy matched). Result is the Rego output
// (JSON); Trace is the Rego evaluation trace (JSON) for ExplainDecision.
// Effect is always one of the PolicyEffect* constants; when no policy
// matches, the default is allow (governance floor — docs/02 §2.5:
// "basic by default").
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

// Evaluate evaluates all published policies at a decision point for the
// given tenant, in narrowest-scope-first order, and returns the first
// definitive decision (docs/02 §2.5). When no published policy matches,
// the default effect is allow (governance floor). The input document is
// the JSON-decoded Rego input.
//
// Each candidate policy's Rego module is evaluated with the input; if
// the query result is `true`, the policy's declared effect is the
// decision. If `false`, the policy does not apply and the next candidate
// is evaluated. An evaluation error on one policy does not abort the
// pass — it is recorded and the next candidate is tried (fail-open to
// allow forward progress; the error is surfaced in the decision record
// and as telemetry).
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
			// Record the error but continue (fail-open — docs/02 §2.5
			// governance floor). The error is surfaced via the decision
			// record when RecordDecision is called.
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
		// Definitive decision: this policy matched (query → true). Apply
		// its declared effect (docs/02 §2.5: first definitive decision
		// wins). Narrowest-scope-first ordering is done by the DB query.
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

// EvaluateVersion evaluates a single specific policy version (dry-run
// for the policy editor / EvaluatePolicy RPC with policy_id set). Used
// to validate an unpublished draft before publish.
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

// evalModule compiles + evaluates a single Rego module against the
// input, capturing the evaluation trace. Returns the Rego result (JSON),
// the trace (JSON), whether the query allowed (true), and any error.
func (e *Engine) evalModule(ctx context.Context, pv db.PolicyVersionRow, input any) (result []byte, trace []byte, allowed bool, err error) {
	query := pv.Query
	if query == "" {
		// Default query: the policy's `allow` rule
		// (data.orchicon.policy.<policy_id>.allow). The package name is
		// derived from the module; if absent, fall back to a generic
		// `data.orchicon.policy.allow`.
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

// RecordDecision persists a PolicyDecision row so ExplainDecision can
// return the Rego trace for a past policy.evaluated event (docs/07 §3.5).
// Called within the caller's transaction so the decision and the state
// mutation it governs commit atomically (AGENTS.md invariant #3).
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

// EnqueuePolicyEvent enqueues a policy.evaluated outbox event so
// streaming clients and webhook subscribers see the decision
// (docs/08 §4.4).
func EnqueuePolicyEvent(ctx context.Context, tx pgx.Tx, eventType string, d db.PolicyDecisionRow) error {
	evt := map[string]any{
		"event_type":      eventType,
		"tenant_id":       d.TenantID,
		"decision_id":     d.ID,
		"policy_id":       d.PolicyID,
		"policy_version":  d.PolicyVersion,
		"decision_point":  d.DecisionPoint,
		"effect":          d.Effect,
		"target_type":     d.TargetType,
		"target_id":       d.TargetID,
		"actor_type":      d.ActorType,
		"actor_id":        d.ActorID,
		"trace_id":        d.TraceID,
		"occurred_at":     d.OccurredAt.Format(time.RFC3339Nano),
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

// CompileModule validates a Rego module compiles (used by the publish
// path to reject malformed Rego before persisting). Returns an error if
// the module does not compile.
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
