// Package web contains the SSR HTTP layer: page rendering, claim endpoints,
// and the SSE bridge into the hub.
package web

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"pixelbeetle/internal/game"
	"pixelbeetle/internal/hub"
)

type Server struct {
	svc    *game.Service
	hub    *hub.Hub
	log    *slog.Logger
	tmpl   *templateRenderer
	secret []byte // HMAC key signing the player_id cookie

	botMu sync.Mutex
	bot   *botReport // latest bot heartbeat posted to /admin/bots
}

// botReport is the heartbeat the load generator POSTs to /admin/bots so the
// dashboard can show end-to-end (API-path) latency alongside the server's own
// counters.
type botReport struct {
	Claims    uint64  `json:"claims"`
	Confirmed uint64  `json:"confirmed"`
	Conflicts uint64  `json:"conflicts"`
	Errors    uint64  `json:"errors"`
	P50Ms     float64 `json:"p50Ms"`
	P99Ms     float64 `json:"p99Ms"`
	RPS       int     `json:"rps"`
}

func New(svc *game.Service, h *hub.Hub, log *slog.Logger, secret string) (*Server, error) {
	tr, err := newTemplateRenderer()
	if err != nil {
		return nil, err
	}
	return &Server{svc: svc, hub: h, log: log, tmpl: tr, secret: []byte(secret)}, nil
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.handleIndex)
	// embed.FS root is the package dir, so files live at "static/..." inside it;
	// sub to that dir so /static/<file> maps directly.
	staticRoot, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic("web: embed static/ missing: " + err.Error())
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticRoot))))
	mux.HandleFunc("GET /sse", s.handleSSE)
	mux.HandleFunc("GET /history", s.handleHistory) // time-travel manifest (client-side slider)
	mux.HandleFunc("POST /claim", s.withPlayer(s.handleClaim))
	mux.HandleFunc("POST /confirm", s.withPlayer(s.handleConfirm))
	mux.HandleFunc("POST /cancel", s.withPlayer(s.handleCancel))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(s.svc.Describe()))
	})
	mux.HandleFunc("POST /admin/bots", s.handleAdminBots) // bot load-generator heartbeat

	return mux
}

// StartMetrics launches a 1s goroutine that rolls the game service's
// throughput counters, merges the latest bot heartbeat and the viewer count,
// and broadcasts the dashboard snapshot over the SSE hub.
func (s *Server) StartMetrics(ctx context.Context) {
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			s.svc.TickMetrics()
			merged := s.svc.MetricsSnapshot()
			merged["viewers"] = s.hub.Count()
			s.botMu.Lock()
			if b := s.bot; b != nil {
				merged["botClaims"] = b.Claims
				merged["botConfirmed"] = b.Confirmed
				merged["botConflicts"] = b.Conflicts
				merged["botErrors"] = b.Errors
				merged["botP50Ms"] = b.P50Ms
				merged["botP99Ms"] = b.P99Ms
				merged["botRps"] = b.RPS
			}
			s.botMu.Unlock()
			s.hub.BroadcastMetrics(merged)
		}
	}()
}

// ---- pages ----

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.renderIndex(w, s.svc); err != nil {
		s.log.Error("render index", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// ---- SSE ----

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	s.hub.ServeSSE(w, r, s.svc.SnapshotBmp)
}

// ---- time travel ----

// handleHistory returns every posted paint (ascending by timestamp) as a
// compact JSON manifest. The client fetches it once, then bisects it locally
// while dragging the slider — no per-tick server round-trip.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"events": s.svc.History()})
}

// ---- claims ----

type claimRequest struct {
	X     uint32 `json:"x"`
	Y     uint32 `json:"y"`
	Color uint8  `json:"color"`
}

type claimResponse struct {
	ClaimID string `json:"claimId"` // 32-char hex of the claim transfer id
}

type idRequest struct {
	ClaimID string `json:"claimId"`
}

func (s *Server) withPlayer(next func(w http.ResponseWriter, r *http.Request, player uuid.UUID)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		player := s.playerFromCookie(w, r)
		next(w, r.WithContext(playerContext(r.Context(), player)), player)
	}
}

func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request, player uuid.UUID) {
	var req claimRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	id, err := s.svc.Claim(player, req.X, req.Y, req.Color)
	switch {
	case errors.Is(err, game.ErrLockedByOther):
		http.Error(w, err.Error(), http.StatusConflict)
	case err != nil:
		s.log.Error("claim", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	default:
		writeJSON(w, claimResponse{ClaimID: hexEncode(id)})
	}
}

// parseClaimID converts a 32-char hex id into bytes.
func parseClaimID(hexID string) ([16]byte, error) {
	var out [16]byte
	b, err := hexDecodeString(hexID)
	if err != nil || len(b) != 16 {
		return out, errors.New("invalid claimId")
	}
	copy(out[:], b)
	return out, nil
}

func (s *Server) handleConfirm(w http.ResponseWriter, r *http.Request, player uuid.UUID) {
	var req idRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	id, err := parseClaimID(req.ClaimID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.svc.Confirm(player, id); err != nil {
		s.failClaim(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request, player uuid.UUID) {
	var req idRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	id, err := parseClaimID(req.ClaimID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.svc.Cancel(player, id); err != nil {
		s.failClaim(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) failClaim(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, game.ErrUnknownClaim):
		http.Error(w, err.Error(), http.StatusNotFound)
	default:
		s.log.Error("resolve claim", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// ---- admin (bot load-generator heartbeat for the dashboard, plan.md §4.1) ----

// handleAdminBots absorbs the load generator's periodic heartbeat. It validates
// nothing beyond JSON shape (demo telemetry); the merged snapshot is pushed to
// browsers over the SSE hub.
func (s *Server) handleAdminBots(w http.ResponseWriter, r *http.Request) {
	var rep botReport
	if !decodeJSON(w, r, &rep) {
		return
	}
	s.botMu.Lock()
	s.bot = &rep
	s.botMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
