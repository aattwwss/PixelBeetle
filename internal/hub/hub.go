// Package hub is the DataStar SSE broadcast fan-out. Every connected browser
// holds one long-lived ServerSentEventGenerator; game events become DataStar
// patch-SIGNALS carrying a base64 bitmap snapshot on connect and batched
// delta arrays during live operation.
//
// The grid is now a <canvas> drawn from a packed byte array (see
// internal/canvas.Bitmap), so the hub no longer sends per-cell HTML element
// patches. It ships the canvas as signals instead:
//
//	on connect:  {"bmp":"<base64 W*H bytes>","locks":[[x,y],...]}
//	live flush:  {"deltas":[[x,y,color],...],"lockAdds":[[x,y],...],"lockRemoves":[[x,y],...]}
//
// Backpressure: each subscriber owns a goroutine + a bounded channel. A slow
// client that stops reading fills its channel and events are DROPPED for that
// client only (never blocking the broadcast to others), and it self-heals on
// the next reconnect snapshot. This is the OMCB "keep it global, tolerate
// stale" principle.
package hub

import (
	"log/slog"
	"net/http"
	"sync"
	"time"

	ds "github.com/starfederation/datastar-go/datastar"
)

const (
	chanSize      = 512
	flushInterval = 50 * time.Millisecond
)

type eventKind uint8

const (
	paintEv eventKind = iota
	lockEv
	unlockEv
)

// event is one fan-out message. color is only meaningful for paintEv.
type event struct {
	kind  eventKind
	x, y  uint32
	color uint8
}

type subscriber struct {
	ch   chan event // bounded; never closed (broadcast sends are non-blocking)
	done chan struct{}
}

// SnapshotFunc returns the current full canvas state for a newly connected
// client: the base64 bitmap plus the cells currently showing a pending lock.
type SnapshotFunc func() (bmpB64 string, locks [][2]uint32)

type Hub struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
	log  *slog.Logger
}

func New(log *slog.Logger) *Hub {
	return &Hub{subs: make(map[*subscriber]struct{}), log: log}
}

// ServeSSE upgrades the request into an SSE stream, sends a snapshot, then
// streams batched deltas until the client disconnects.
func (h *Hub) ServeSSE(w http.ResponseWriter, r *http.Request, snapshot SnapshotFunc) {
	gen := ds.NewSSE(w, r)
	sub := &subscriber{ch: make(chan event, chanSize), done: make(chan struct{})}

	h.mu.Lock()
	h.subs[sub] = struct{}{}
	n := len(h.subs)
	h.mu.Unlock()
	h.log.Debug("sse subscriber connected", "total", n)

	// The flusher goroutine owns all writes to gen (snapshot + flushes), so a
	// connection is never written from two goroutines.
	flusherDone := make(chan struct{})
	go func() {
		defer close(flusherDone)
		sub.run(gen, snapshot, h.log)
	}()

	<-r.Context().Done() // stream lives until client hangs up

	h.mu.Lock()
	delete(h.subs, sub)
	h.mu.Unlock()
	close(sub.done) // tell the flusher to stop
	<-flusherDone   // and wait for it to finish writing
}

func (s *subscriber) run(gen *ds.ServerSentEventGenerator, snapshot SnapshotFunc, log *slog.Logger) {
	// Snapshot first so the client is correct even if it missed deltas while
	// disconnected (backgrounded tab, etc.).
	if snapshot != nil {
		bmp, locks := snapshot()
		if err := gen.MarshalAndPatchSignals(map[string]any{"bmp": bmp, "locks": locks}); err != nil {
			return // client gone before snapshot finished
		}
	}

	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	var deltas [][3]uint32
	var lockAdds, lockRemoves [][2]uint32

	flush := func() bool {
		if len(deltas)+len(lockAdds)+len(lockRemoves) == 0 {
			return true
		}
		// All three keys are ALWAYS present (possibly empty) because JSON Merge
		// Patch leaves absent keys untouched — omitting lockRemoves here would
		// leave a stale array from an earlier flush and re-apply it.
		payload := map[string]any{
			"deltas":      deltas,
			"lockAdds":    lockAdds,
			"lockRemoves": lockRemoves,
		}
		if err := gen.MarshalAndPatchSignals(payload); err != nil {
			return false // client gone
		}
		deltas, lockAdds, lockRemoves = nil, nil, nil
		return true
	}

	for {
		select {
		case <-s.done:
			return
		case ev := <-s.ch:
			switch ev.kind {
			case paintEv:
				// Delta color is the paint byte (1..16 = palette index + 1), the
				// same encoding as the bitmap: 0 is reserved for "empty".
				deltas = append(deltas, [3]uint32{ev.x, ev.y, uint32(ev.color%16) + 1})
			case lockEv:
				lockAdds = append(lockAdds, [2]uint32{ev.x, ev.y})
			case unlockEv:
				lockRemoves = append(lockRemoves, [2]uint32{ev.x, ev.y})
			}
		case <-ticker.C:
			if !flush() {
				return
			}
		}
	}
}

// BroadcastPaint enqueues a paint delta. color is the palette index (0..15);
// the wire delta carries color+1 so byte 0 stays reserved for "empty".
func (h *Hub) BroadcastPaint(x, y uint32, color uint8) {
	h.broadcast(event{kind: paintEv, x: x, y: y, color: color})
}

// BroadcastLock enqueues a pending-lock overlay add.
func (h *Hub) BroadcastLock(x, y uint32) {
	h.broadcast(event{kind: lockEv, x: x, y: y})
}

// BroadcastUnlock enqueues a pending-lock overlay remove.
func (h *Hub) BroadcastUnlock(x, y uint32) {
	h.broadcast(event{kind: unlockEv, x: x, y: y})
}

func (h *Hub) broadcast(ev event) {
	h.mu.Lock()
	subs := make([]*subscriber, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()

	dropped := 0
	for _, s := range subs {
		select {
		case s.ch <- ev:
		default:
			dropped++ // slow consumer: drop for this client only
		}
	}
	if dropped > 0 {
		h.log.Debug("hub: dropped events for slow subscribers", "dropped", dropped)
	}
}

// Count returns the number of connected subscribers.
func (h *Hub) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}
