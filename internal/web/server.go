// Package web contains the SSR HTTP layer: page rendering, claim endpoints,
// and the SSE bridge into the hub.
package web

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"pixelbeetle/internal/game"
	"pixelbeetle/internal/hub"
	"pixelbeetle/internal/tbclient"
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
	mux.HandleFunc("GET /api/canvas", s.handleCanvas)   // current canvas state for `bot -preview`
	mux.HandleFunc("GET /history", s.handleHistoryPage) // timelapse view (separate page; the canvas homepage stays history-free)
	mux.HandleFunc("GET /api/history/meta", s.handleHistoryMeta)
	mux.HandleFunc("GET /api/history/frame", s.handleHistoryFrame)
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

// handleCanvas returns the current full canvas state — base64 packed bitmap,
// active locks, grid dims — the same snapshot an SSE subscriber receives on
// connect. Lets `bot -preview` overlay a blueprint and show collisions
// before painting a single claim.
func (s *Server) handleCanvas(w http.ResponseWriter, r *http.Request) {
	bmp, locks := s.svc.SnapshotBmp()
	gw, gh := s.svc.Grid()
	writeJSON(w, map[string]any{
		"gridW": gw,
		"gridH": gh,
		"bmp":   bmp,
		"locks": locks,
	})
}

// ---- history (timelapse view) ----

// handleHistoryPage serves the timelapse view: a slider whose every stop is
// a live TigerBeetle query (anchor bitmap + at most one minute of events).
func (s *Server) handleHistoryPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.renderHistory(w, s.svc); err != nil {
		s.log.Error("render history", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// handleHistoryMeta returns the timeline bounds for the slider.
func (s *Server) handleHistoryMeta(w http.ResponseWriter, r *http.Request) {
	minMs, maxMs, intervalMs := s.svc.HistoryMeta()
	gw, gh := s.svc.Grid()
	writeJSON(w, map[string]any{
		"minMs":      minMs,
		"maxMs":      maxMs,
		"intervalMs": intervalMs,
		"gridW":      gw,
		"gridH":      gh,
	})
}

// handleHistoryFrame renders the canvas at one instant — one live TB query
// per request (anchor fold + at most one minute of events). ts_ms comes in
// as a query param; the slider quantizes to it client-side.
func (s *Server) handleHistoryFrame(w http.ResponseWriter, r *http.Request) {
	ts, err := strconv.ParseInt(r.URL.Query().Get("ts_ms"), 10, 64)
	if err != nil || ts < 0 {
		http.Error(w, "ts_ms must be a non-negative integer (ms since epoch)", http.StatusBadRequest)
		return
	}
	bmp, effTs, err := s.svc.FrameAt(ts)
	if err != nil {
		if errors.Is(err, game.ErrFrameTooBroad) {
			// The seek needs more than maxFramePages TB round-trips (no
			// checkpoint near the point, or an unusually dense era): tell the
			// client to pick a newer time instead of hammering the cluster.
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.log.Error("history frame", "err", err, "tsMs", ts)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"tsMs": effTs, "bmp": bmp})
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
	if req.Color > tbclient.MaxColor {
		http.Error(w, fmt.Sprintf("color must be 0-%d", tbclient.MaxColor), http.StatusBadRequest)
		return
	}
	id, err := s.svc.Claim(player, req.X, req.Y, req.Color)
	switch {
	case errors.Is(err, game.ErrOutOfBounds):
		http.Error(w, err.Error(), http.StatusBadRequest)
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
	case errors.Is(err, tbclient.ErrClaimExpired):
		// The pending claim timed out in TB before this leg arrived — a normal
		// outcome, not a server fault. The cell is already claimable again.
		http.Error(w, "claim expired, cell released", http.StatusGone)
	default:
		s.log.Error("resolve claim", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// ---- admin (bot load-generator heartbeat feeding the live metrics dashboard) ----

// handleAdminBots absorbs the load generator's periodic heartbeat. It validates
// nothing beyond JSON shape and is unauthenticated — the merged snapshot is
// pushed to browsers over the SSE hub. Fine for a localhost demo; gate behind a
// shared token before exposing the dashboard in any real deployment.
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
