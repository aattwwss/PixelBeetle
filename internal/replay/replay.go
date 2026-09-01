// Package replay consumes the TigerBeetle CDC stream (AMQP 0.9.1, i.e.
// RabbitMQ) produced by `tigerbeetle amqp`. It parses each event, dedupes by
// transfer id (CDC is at-least-once), and hands events to a Sink.
//
// The game server uses this two ways: a live subscriber keeps the pixel cache
// in sync across instances, and the same Event type is the shape a boot-time
// warm-up could replay from timestamp 0.
package replay

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	tb "github.com/tigerbeetle/tigerbeetle-go"
)

type EventType string

const (
	TypeSingle  EventType = "single_phase"
	TypePending EventType = "two_phase_pending"
	TypePosted  EventType = "two_phase_posted"
	TypeVoided  EventType = "two_phase_voided"
	TypeExpired EventType = "two_phase_expired"
)

// Event is a normalized canvas event. X/Y/Color are populated for claim
// events (pending and posted); other events carry zero values there.
type Event struct {
	Type       EventType
	Timestamp  uint64 // nanoseconds, from the message body (AMQP header truncates to seconds)
	X, Y       uint32
	Color      uint8
	Player     uuid.UUID
	TransferID tb.Uint128
}

// Sink receives deduplicated events in delivery order.
type Sink interface {
	ApplyEvent(Event)
}

type Config struct {
	AMQPURL  string // amqp://guest:guest@localhost:5672/
	Exchange string // must pre-exist (declared by scripts/rabbitmq.sh)
	Log      *slog.Logger
}

type Consumer struct {
	cfg  Config
	sink Sink
	log  *slog.Logger
}

func NewConsumer(cfg Config, sink Sink) *Consumer {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Consumer{cfg: cfg, sink: sink, log: log}
}

// reconnectBackoffMin is the initial wait after a connection error before
// re-dialing; reconnectBackoffMax caps its exponential growth.
const (
	reconnectBackoffMin = time.Second
	reconnectBackoffMax = 30 * time.Second
)

// Run consumes the CDC stream until ctx is cancelled, reconnecting with
// capped exponential backoff after transient connection errors, so a single
// RabbitMQ blip can't silently end the pipeline. Returns ctx.Err() on
// cancellation — a clean stop, not a failure.
func (c *Consumer) Run(ctx context.Context) error {
	backoff := reconnectBackoffMin
	for {
		err := c.runOnce(ctx)
		if err == nil || ctx.Err() != nil {
			return ctx.Err()
		}
		c.log.Warn("replay: connection lost; reconnecting", "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < reconnectBackoffMax {
			backoff *= 2
		}
	}
}

// runOnce dials, binds a per-instance exclusive queue to the fanout exchange,
// and consumes until the context is cancelled or the connection breaks.
func (c *Consumer) runOnce(ctx context.Context) error {
	conn, err := amqp.Dial(c.cfg.AMQPURL)
	if err != nil {
		return fmt.Errorf("replay: dial: %w", err)
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("replay: channel: %w", err)
	}
	defer ch.Close()

	// The CDC job requires the exchange to already exist — verify, don't create.
	if err := ch.ExchangeDeclarePassive(c.cfg.Exchange, "fanout", true, false, false, false, nil); err != nil {
		return fmt.Errorf("replay: exchange %q must pre-exist: %w", c.cfg.Exchange, err)
	}

	// Server-named, exclusive, auto-delete queue: each instance gets its own
	// copy of the fanout and the queue disappears on disconnect. A shared
	// named queue would make a second instance fail with RESOURCE_LOCKED.
	q, err := ch.QueueDeclare("", false, true, true, false, nil)
	if err != nil {
		return fmt.Errorf("replay: queue declare: %w", err)
	}
	if err := ch.QueueBind(q.Name, "", c.cfg.Exchange, false, nil); err != nil {
		return fmt.Errorf("replay: queue bind: %w", err)
	}

	deliveries, err := ch.Consume(q.Name, "", false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("replay: consume: %w", err)
	}

	// Per-connection dedupe: within one stream AMQP is at-least-once, so the
	// same transfer id can be redelivered. No size cap — the bound is
	// inherited from claim volume, and a wholesale reset would re-apply old
	// redeliveries (a stale replay can regress a newer paint).
	seen := make(map[string]struct{})
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case d, ok := <-deliveries:
			if !ok {
				return fmt.Errorf("replay: delivery channel closed")
			}
			c.processBody(d.Body, seen)
			// Ack after processing (bad messages and duplicates included): an
			// unacked poison-pill message would otherwise redeliver forever.
			_ = d.Ack(false)
		}
	}
}

// processBody parses, dedupes and applies one CDC message body. Parse
// failures are logged and skipped (the caller's unconditional ack clears
// them). Kept as a separate method so it can be tested without a broker.
func (c *Consumer) processBody(body []byte, seen map[string]struct{}) {
	ev, err := ParseMessage(body)
	if err != nil {
		c.log.Warn("replay: bad message (acked to avoid poison-pill loop)", "err", err)
		return
	}
	key := ev.TransferID.String()
	if key != "" {
		if _, dup := seen[key]; dup {
			return // redelivered; already applied
		}
		seen[key] = struct{}{}
	}
	c.sink.ApplyEvent(ev)
}
