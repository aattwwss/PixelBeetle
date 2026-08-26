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
	Exchange string // must pre-exist (declared by scripts/dev-rabbit.sh)
	Queue    string // optional; empty uses a server-named queue
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

// Run connects, verifies the exchange, consumes, and pumps events until ctx is
// cancelled or a fatal connection error occurs. It does not auto-reconnect:
// callers that want resilience should restart it on error.
func (c *Consumer) Run(ctx context.Context) error {
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

	queueName := c.cfg.Queue
	q, err := ch.QueueDeclare(queueName, false, true, true, false, nil)
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

	seen := make(map[string]struct{})
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case d, ok := <-deliveries:
			if !ok {
				return fmt.Errorf("replay: delivery channel closed")
			}
			ev, err := ParseMessage(d.Body)
			if err != nil {
				c.log.Warn("replay: bad message (acked to avoid poison-pill loop)", "err", err)
				_ = d.Ack(false)
				continue
			}
			key := ev.TransferID.String()
			if key != "" {
				if _, dup := seen[key]; dup {
					_ = d.Ack(false)
					continue
				}
				if len(seen) > 1_000_000 {
					seen = make(map[string]struct{})
				}
				seen[key] = struct{}{}
			}
			c.sink.ApplyEvent(ev)
			_ = d.Ack(false)
		}
	}
}
