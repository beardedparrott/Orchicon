// Package eventbus is the thin NATS JetStream publisher and subscriber used by the outbox relay and
// streaming RPCs (docs/08_Event_Bus_and_Telemetry_Model.md §2).
package eventbus

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Publisher is the interface the outbox relay depends on.
type Publisher interface {
	// Publish emits one event.
	Publish(ctx context.Context, subject, msgID string, data []byte) error
}

// Subscriber is the interface streaming RPCs depend on.
type Subscriber interface {
	// Subscribe creates a consumer on the ORCHICON_EVENTS stream filtered to the given subject filter.
	Subscribe(ctx context.Context, filter string, fromSeq uint64) (<-chan EventMsg, error)
}

// EventMsg is a NATS message projected to the streaming RPC layer.
type EventMsg struct {
	Subject string
	Seq     uint64
	Data    []byte
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

// NewNATSPublisher connects to NATS at url and returns a publisher that publishes to JetStream.
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
		Name:       "ORCHICON_EVENTS",
		Subjects:   []string{"orchicon.events.>"},
		Storage:    jetstream.FileStorage,
		Retention:  jetstream.LimitsPolicy,
		Discard:    jetstream.DiscardOld,
		MaxAge:     72 * time.Hour,
		Duplicates: 5 * time.Minute,
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

// NATSSubscriber subscribes to NATS JetStream subjects for streaming
// RPCs. Each call to Subscribe creates an ephemeral consumer.
type NATSSubscriber struct {
	nc *nats.Conn
	js jetstream.JetStream
}

// NewNATSSubscriber connects to NATS and returns a subscriber.
func NewNATSSubscriber(ctx context.Context, url string) (*NATSSubscriber, error) {
	nc, err := nats.Connect(url,
		nats.Name("orchicon-stream-subscriber"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("eventbus: connect nats subscriber: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("eventbus: new jetstream subscriber: %w", err)
	}
	return &NATSSubscriber{nc: nc, js: js}, nil
}

// Subscribe implements Subscriber.
func (s *NATSSubscriber) Subscribe(ctx context.Context, filter string, fromSeq uint64) (<-chan EventMsg, error) {
	cfg := jetstream.ConsumerConfig{
		Durable:       "",
		FilterSubject: filter,
		AckPolicy:     jetstream.AckExplicitPolicy,
	}
	if fromSeq > 0 {
		cfg.OptStartSeq = fromSeq
		cfg.DeliverPolicy = jetstream.DeliverByStartSequencePolicy
	} else {
		// Skip the entire prior stream — only deliver new events.
		cfg.DeliverPolicy = jetstream.DeliverNewPolicy
	}

	cons, err := s.js.CreateOrUpdateConsumer(ctx, "ORCHICON_EVENTS", cfg)
	if err != nil {
		return nil, fmt.Errorf("eventbus: create consumer: %w", err)
	}

	ch := make(chan EventMsg, 64)

	consCtx, err := cons.Consume(func(msg jetstream.Msg) {
		meta, mErr := msg.Metadata()
		if mErr != nil {
			return
		}
		em := EventMsg{
			Subject: msg.Subject(),
			Seq:     meta.Sequence.Stream,
			Data:    msg.Data(),
		}
		select {
		case ch <- em:
			_ = msg.Ack()
		case <-ctx.Done():
			_ = msg.Nak()
		}
	})
	if err != nil {
		return nil, fmt.Errorf("eventbus: start consumer: %w", err)
	}

	go func() {
		defer close(ch)
		<-ctx.Done()
		consCtx.Stop()
		// Drain any in-flight message from the NATS callback before closing the channel.
		<-consCtx.Closed()
	}()

	return ch, nil
}
