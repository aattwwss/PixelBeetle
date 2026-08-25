// Package replay will consume the TigerBeetle CDC stream (RabbitMQ / AMQP
// 0.9.1) to build the time-ordered claim log behind the time-travel slider,
// and to warm the pixel cache on server cold-start (plan.md §3).
//
// Scaffold stub: the AMQP wiring lands in build-order step 4. The consumer
// must dedupe by transfer id — CDC is at-least-once.
package replay

import (
	"context"
	"errors"
	"time"
)

var ErrNotImplemented = errors.New("replay: CDC consumer not implemented yet")

type Event struct {
	Type      string // two_phase_pending | posted | voided | expired
	Timestamp uint64 // nanoseconds, from message body (NOT AMQP header — truncated)
	X, Y      uint32
	Color     uint8
	Player    [16]byte
}

type Consumer interface {
	OnEvent(e Event) error
}

type Config struct {
	AMQPURL       string        // amqp://guest:guest@localhost:5672/
	Exchange      string        // must pre-exist; see docker-compose rabbitmq-init
	RoutingKey    string        // optional if exchange is set
	FromTimestamp uint64        // --timestamp-last equivalent for catch-up
	DedupeWindow  time.Duration // recent transfer-id cache TTL
}

// Start connects and pumps events into c until ctx is cancelled.
func Start(ctx context.Context, cfg Config, c Consumer) error {
	<-ctx.Done()
	return ErrNotImplemented
}
