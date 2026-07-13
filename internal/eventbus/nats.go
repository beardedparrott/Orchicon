// Package eventbus is the thin NATS JetStream publisher used by the
// outbox relay (docs/08_Event_Bus_and_Telemetry_Model.md §2). No
// component other than the relay publishes to NATS (AGENTS.md
// invariant #3), and the relay only publishes events that were already
// committed to the outbox table.
package eventbus

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Publisher is the interface the outbox relay depends on. It is
// satisfied by the NATS JetStream publisher; tests may substitute a
// fake.
type Publisher interface {
	// Publish emits one event. MsgId is used for JetStream deduplication
	// so concurrent relays publishing the same outbox row are idempotent
	// (docs/09 §6). Subject is derived from the aggregate/event type.
	Publish(ctx context.Context, subject, msgID string, data []byte) error
}

// SubjectFor returns the NATS subject for an event. Events are
// partitioned by aggregate type so consumers can subscribe narrowly:
//
//	orchicon.events.<aggregate_type>.<event_type>
//
// e.g. orchicon.events.project.created
func SubjectFor(aggregateType, eventType string) string {
	return fmt.Sprintf("orchicon.events.%s.%s", aggregateType, eventType)
}

// NATSPublisher publishes events to a NATS JetStream stream.
type NATSPublisher struct {
	js jetstream.JetStream
}

// NewNATSPublisher connects to NATS at url and returns a publisher that
// publishes to JetStream. The stream "ORCHICON_EVENTS" is created if it
// does not exist (idempotent). The connection is lazy; a failed publish
// surfaces the error to the relay, which retries on the next poll.
func NewNATSPublisher(ctx context.Context, url string) (*NATSPublisher, error) {
	nc, err := nats.Connect(url,
		nats.Name("orchicon-outbox-relay"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("eventbus: connect nats: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("eventbus: new jetstream: %w", err)
	}
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:        "ORCHICON_EVENTS",
		Subjects:    []string{"orchicon.events.>"},
		Storage:     jetstream.FileStorage,
		Retention:   jetstream.LimitsPolicy,
		Discard:     jetstream.DiscardOld,
		MaxAge:      72 * time.Hour,
		Duplicates:  5 * time.Minute,
	}); err != nil {
		return nil, fmt.Errorf("eventbus: create stream: %w", err)
	}
	return &NATSPublisher{js: js}, nil
}

// Publish implements Publisher. MsgId enables JetStream deduplication.
func (p *NATSPublisher) Publish(ctx context.Context, subject, msgID string, data []byte) error {
	_, err := p.js.Publish(ctx, subject, data, jetstream.WithMsgID(msgID))
	if err != nil {
		return fmt.Errorf("eventbus: publish %s: %w", subject, err)
	}
	return nil
}
