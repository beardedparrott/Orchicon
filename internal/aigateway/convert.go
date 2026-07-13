package aigateway

import (
	"encoding/json"
	"errors"
	"time"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// defaultProviders returns the LLM providers known to the gateway for
// v0.1 (docs/01 §2, docs/04 §6). The OpenCode adapter routes to these
// providers; the gateway records usage + cost from adapter telemetry.
func defaultProviders() []*apiv1.AIProvider {
	return []*apiv1.AIProvider{
		{
			Id:      "anthropic",
			Name:    "Anthropic",
			Enabled: true,
			Models:  []string{"claude-sonnet-4", "claude-opus-4", "claude-haiku-4"},
		},
		{
			Id:      "openai",
			Name:    "OpenAI",
			Enabled: true,
			Models:  []string{"gpt-4.1", "o3", "gpt-4o-mini"},
		},
		{
			Id:      "local",
			Name:    "Local / Free",
			Enabled: true,
			Models:  []string{"opencode/deepseek-v4-flash-free", "local/llama"},
		},
	}
}

// tsToTime converts an optional protobuf timestamp to a time.Time.
// Returns the zero time if nil, which the data-access layer treats as
// "no bound" via the 'epoch' sentinel in parameterized queries.
func tsToTime(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime()
}

func usageRowToProto(r *db.UsageRecordRow) *apiv1.UsageRecord {
	return &apiv1.UsageRecord{
		Id:               r.ID,
		TenantId:         r.TenantID,
		ProjectId:        r.ProjectID,
		TaskId:           r.TaskID,
		ExecutionId:      r.ExecutionID,
		WorkerId:         r.WorkerID,
		Provider:         r.Provider,
		Model:            r.Model,
		PromptTokens:     r.PromptTokens,
		CompletionTokens: r.CompletionTokens,
		TotalTokens:      r.TotalTokens,
		CostUsd:          r.CostUSD,
		CorrelationId:    r.CorrelationID,
		OccurredAt:       timestamppb.New(r.OccurredAt),
	}
}

func costRowToProto(r *db.CostSummaryRow, groupBy, parentKey string, start, end time.Time) *apiv1.CostSummary {
	return &apiv1.CostSummary{
		GroupBy:           groupBy,
		GroupKey:          r.GroupKey,
		ParentKey:         parentKey,
		TotalTokens:       r.TotalTokens,
		PromptTokens:      r.PromptTokens,
		CompletionTokens:  r.CompletionTokens,
		CostUsd:           r.CostUSD,
		ExecutionCount:    r.ExecutionCount,
		RecordCount:      r.RecordCount,
		WindowStart:       timestamppb.New(start),
		WindowEnd:         timestamppb.New(end),
	}
}

// parseUsageEvent decodes a NATS usage event payload (the outbox envelope
// payload for an "usage.recorded" event) into the wire UsageEvent proto.
// The payload is the JSON event envelope's `payload` field re-encoded
// as the UsageEvent shape.
func parseUsageEvent(data []byte) (*apiv1.UsageEvent, error) {
	if len(data) == 0 {
		return nil, errors.New("empty usage event payload")
	}
	var raw struct {
		ID               string  `json:"id"`
		TenantID         string  `json:"tenant_id"`
		ProjectID        string  `json:"project_id"`
		TaskID           string  `json:"task_id"`
		ExecutionID      string  `json:"execution_id"`
		WorkerID         string  `json:"worker_id"`
		Provider         string  `json:"provider"`
		Model            string  `json:"model"`
		PromptTokens     int64   `json:"prompt_tokens"`
		CompletionTokens int64   `json:"completion_tokens"`
		TotalTokens      int64   `json:"total_tokens"`
		CostUSD          float64 `json:"cost_usd"`
		CorrelationID    string  `json:"correlation_id"`
		OccurredAt       string  `json:"occurred_at"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	evt := &apiv1.UsageEvent{
		Id:               raw.ID,
		TenantId:         raw.TenantID,
		ProjectId:        raw.ProjectID,
		TaskId:           raw.TaskID,
		ExecutionId:      raw.ExecutionID,
		WorkerId:         raw.WorkerID,
		Provider:         raw.Provider,
		Model:            raw.Model,
		PromptTokens:     raw.PromptTokens,
		CompletionTokens: raw.CompletionTokens,
		TotalTokens:      raw.TotalTokens,
		CostUsd:          raw.CostUSD,
		CorrelationId:    raw.CorrelationID,
	}
	if raw.OccurredAt != "" {
		if t, err := time.Parse(time.RFC3339Nano, raw.OccurredAt); err == nil {
			evt.OccurredAt = timestamppb.New(t)
		}
	}
	return evt, nil
}

// usageEventEnvelope is the payload shape published to the
// orchicon.events.usage.> subject via the outbox (docs/08 §5.2). It
// mirrors a usage_records row.
type usageEventEnvelope struct {
	ID               string  `json:"id"`
	TenantID         string  `json:"tenant_id"`
	ProjectID        string  `json:"project_id"`
	TaskID           string  `json:"task_id"`
	ExecutionID      string  `json:"execution_id"`
	WorkerID         string  `json:"worker_id"`
	Provider         string  `json:"provider"`
	Model            string  `json:"model"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	CorrelationID    string  `json:"correlation_id"`
	OccurredAt       string  `json:"occurred_at"`
}

func usageEventPayload(r *db.UsageRecordRow) []byte {
	e := usageEventEnvelope{
		ID:               r.ID,
		TenantID:         r.TenantID,
		ProjectID:        r.ProjectID,
		TaskID:           r.TaskID,
		ExecutionID:      r.ExecutionID,
		WorkerID:         r.WorkerID,
		Provider:         r.Provider,
		Model:            r.Model,
		PromptTokens:     r.PromptTokens,
		CompletionTokens: r.CompletionTokens,
		TotalTokens:      r.TotalTokens,
		CostUSD:          r.CostUSD,
		CorrelationID:    r.CorrelationID,
		OccurredAt:       r.OccurredAt.UTC().Format(time.RFC3339Nano),
	}
	b, _ := json.Marshal(e)
	return b
}
