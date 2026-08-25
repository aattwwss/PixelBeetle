// Package web contains the SSR HTTP layer: page rendering, claim endpoints,
// and the SSE bridge into the hub.
package web

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"pixelbeetle/internal/game"
	"pixelbeetle/internal/hub"
)

type Server struct {
	svc  *game.Service
	hub  *hub.Hub
	log  *slog.Logger
	tmpl *templateRenderer
}

func New(svc *game.Service, h *hub.Hub, log *slog.Logger) (*Server, error) {
	tr, err := newTemplateRenderer()
	if err != nil {
		return nil, err
	}
	return &Server{svc: svc, hub: h, log: log, tmpl: tr}, nil
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
	mux.HandleFunc("GET /sse", s.handleSSE)
	mux.HandleFunc("POST /claim", s.withPlayer(s.handleClaim))
	mux.HandleFunc("POST /confirm", s.withPlayer(s.handleConfirm))
	mux.HandleFunc("POST /cancel", s.withPlayer(s.handleCancel))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(s.svc.Describe()))
	})
	mux.HandleFunc("POST /admin/bots", s.handleAdminBots) // reserved; see plan.md §4.1

	return mux
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
	s.hub.ServeSSE(w, r, nil) // initialSync wired in main once cache warm-up lands
}

// ---- claims ----

type claimRequest struct {
	X     uint32 `json:"x"`
	Y     uint32 `json:"y"`
	Color uint8  `json:"color"`
}

type claimResponse struct {
	ClaimID [16]byte `json:"claimId"`
}

type idRequest struct {
	ClaimID [16]byte `json:"claimId"`
}

func (s *Server) withPlayer(next func(w http.ResponseWriter, r *http.Request, player uuid.UUID)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		player := playerFromCookie(w, r)
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
		writeJSON(w, claimResponse{ClaimID: id})
	}
}

func (s *Server) handleConfirm(w http.ResponseWriter, r *http.Request, player uuid.UUID) {
	var req idRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.svc.Confirm(player, req.ClaimID); err != nil {
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
	if err := s.svc.Cancel(player, req.ClaimID); err != nil {
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

// ---- admin (reserved for live bot control, plan.md §4.1) ----

func (s *Server) handleAdminBots(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "not implemented yet", http.StatusNotImplemented)
}
