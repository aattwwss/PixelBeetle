package web

import (
	"context"
	"embed"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"pixelbeetle/internal/canvas"
	"pixelbeetle/internal/game"
)

//go:embed static/*
var staticFS embed.FS

//go:embed templates/index.html
var indexHTML embed.FS

const playerCookie = "player_id"

// ---- player identity (stateless cookie for the demo) ----
// TODO(plan): HMAC-sign before any public deployment.

type ctxKey int

const playerKey ctxKey = 0

func playerFromCookie(w http.ResponseWriter, r *http.Request) uuid.UUID {
	if c, err := r.Cookie(playerCookie); err == nil {
		if u, err := uuid.Parse(c.Value); err == nil {
			return u
		}
	}
	u := uuid.Must(uuid.NewV7())
	http.SetCookie(w, &http.Cookie{
		Name:    playerCookie,
		Value:   u.String(),
		Path:    "/",
		Expires: time.Now().Add(365 * 24 * time.Hour),
		Secure:  false, // demo runs on localhost
	})
	return u
}

func playerContext(ctx context.Context, u uuid.UUID) context.Context {
	return context.WithValue(ctx, playerKey, u)
}

// ---- templates ----

type templateRenderer struct {
	index *template.Template
}

func newTemplateRenderer() (*templateRenderer, error) {
	funcs := template.FuncMap{
		"cellID":   canvas.CellID,
		"colorCSS": canvas.ColorCSS,
		"cellHTML": canvas.CellHTML,
	}
	idx, err := template.New("index.html").Funcs(funcs).ParseFS(indexHTML, "templates/index.html")
	if err != nil {
		return nil, err
	}
	return &templateRenderer{index: idx}, nil
}

func (tr *templateRenderer) renderIndex(w io.Writer, svc *game.Service) error {
	snap := svc.Snapshot()
	gw, gh := svc.Grid()
	cells := make([]template.HTML, 0, int(gw)*int(gh))
	for y := uint32(0); y < gh; y++ {
		for x := uint32(0); x < gw; x++ {
			if p, ok := snap[uint64(x)<<32|uint64(y)]; ok { // CellHTML is trusted markup built from ints only
				cells = append(cells, template.HTML(canvas.CellHTML(x, y, "painted", p.Color)))
			} else {
				cells = append(cells, template.HTML(canvas.CellHTML(x, y, "", 0)))
			}
		}
	}
	return tr.index.ExecuteTemplate(w, "index.html", map[string]any{
		"Cells": cells,
		"Cols":  gw,
	})
}

// ---- misc helpers ----

func decodeJSON[T any](w http.ResponseWriter, r *http.Request, out *T) bool {
	defer func() { _, _ = io.Copy(io.Discard, r.Body) }()
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func hexEncode(b [16]byte) string { return hex.EncodeToString(b[:]) }

func hexDecodeString(s string) ([]byte, error) { return hex.DecodeString(s) }
