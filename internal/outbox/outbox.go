// Package outbox implements the transactional outbox pattern (docs/09_Database_Schema.md §6,
// docs/08_Event_Bus_and_Telemetry_Model.md §2.4).
package outbox

import "time"

// Event is the in-memory representation of an outbox row before it is persisted and published.
type Event struct {
	EventType     string
	AggregateType string
	AggregateID   string
	AggregateVer  int
	TenantID      string
	OccurredAt    time.Time
	TraceID       string
	CorrelationID string
	Payload       []byte // protobuf-encoded, per docs/08 §9
}
