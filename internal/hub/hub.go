// Package hub is the DataStar SSE broadcast fan-out. Every connected browser
// holds one long-lived ServerSentEventGenerator; game events become tiny
// element patches targeted by cell id.
package hub

import (
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	ds "github.com/starfederation/datastar-go/datastar"

	"pixelbeetle/internal/canvas"
)

type subscriber struct {
	sse *ds.ServerSentEventGenerator
	mu  sync.Mutex // serializes writes on this one connection
}

func (s *subscriber) send(fn func(gen *ds.ServerSentEventGenerator) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sse.IsClosed() {
		return
	}
	_ = fn(s.sse)
}

type Hub struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
	log  *slog.Logger
}

func New(log *slog.Logger) *Hub {
	return &Hub{subs: make(map[*subscriber]struct{}), log: log}
}

// ServeSSE upgrades the request into an SSE stream, performs an initial full
// canvas sync, then blocks streaming events until the client disconnects.
// initialSync renders every non-empty cell as one batched patch event.
func (h *Hub) ServeSSE(w http.ResponseWriter, r *http.Request, initialSync func(yield func(x, y uint32, state string, color uint8))) {
	gen := ds.NewSSE(w, r)
	sub := &subscriber{sse: gen}

	h.mu.Lock()
	h.subs[sub] = struct{}{}
	n := len(h.subs)
	h.mu.Unlock()
	h.log.Debug("sse subscriber connected", "total", n)

	defer func() {
		h.mu.Lock()
		delete(h.subs, sub)
		h.mu.Unlock()
	}()

	// Full-canvas diff on (re)connect — clients never need a manual resync.
	if initialSync != nil {
		var sb []byte
		initialSync(func(x, y uint32, state string, color uint8) {
			sb = append(sb, canvas.CellHTML(x, y, state, color)...)
		})
		if len(sb) > 0 {
			sub.send(func(g *ds.ServerSentEventGenerator) error {
				return g.PatchElements(string(sb))
			})
		}
	}

	<-r.Context().Done() // stream lives until client hangs up
}

// PixelPainted permanently paints a cell (posted claim).
func (h *Hub) PixelPainted(x, y uint32, color uint8) {
	el := canvas.CellHTML(x, y, "painted", color)
	h.broadcast(func(g *ds.ServerSentEventGenerator) error {
		return g.PatchElements(el)
	})
}

// PixelLocked marks a cell as claimed-pending (lock acquired).
func (h *Hub) PixelLocked(x, y uint32) {
	el := canvas.CellHTML(x, y, "locked", 0)
	h.broadcast(func(g *ds.ServerSentEventGenerator) error {
		return g.PatchElements(el)
	})
}

// PixelUnlocked reverts a cell to its cache-derived appearance.
func (h *Hub) PixelUnlocked(x, y uint32) {
	el := canvas.CellHTML(x, y, "", 0)
	h.broadcast(func(g *ds.ServerSentEventGenerator) error {
		return g.PatchElements(el)
	})
}

func (h *Hub) broadcast(fn func(*ds.ServerSentEventGenerator) error) {
	h.mu.Lock()
	subs := make([]*subscriber, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()

	for _, s := range subs {
		s.send(fn)
	}
}

func (h *Hub) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

var _ = fmt.Sprintf // silence unused import churn while scaffold evolves
