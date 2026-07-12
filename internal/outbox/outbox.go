// Package outbox implements the transactional outbox pattern
// (docs/09_Database_Schema.md §6, docs/08_Event_Bus_and_Telemetry_Model.md §2.4).
//
// Every control-plane mutation writes a row to the outbox table in the
// same transaction as the state change. A background relay polls the
// table and publishes events to NATS JetStream, then marks rows
// published. Delivery is at-least-once; consumers must be idempotent.
//
// No component other than the relay publishes to NATS, and no mutation
// path skips the outbox (AGENTS.md invariant #3).
package outbox

import "time"

// Event is the in-memory representation of an outbox row before it is
// persisted and published. The envelope shape is fixed by
// docs/08_Event_Bus_and_Telemetry_Model.md §3.
type Event struct {
	EventType      string
	AggregateType  string
	AggregateID    string
	AggregateVer   int
	TenantID       string
	OccurredAt     time.Time
	TraceID        string
	CorrelationID  string
	Payload        []byte // protobuf-encoded, per docs/08 §9
}
