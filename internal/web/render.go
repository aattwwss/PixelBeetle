package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"pixelbeetle/internal/game"
)

//go:embed static/*
var staticFS embed.FS

//go:embed templates/index.html
var indexHTML embed.FS

const playerCookie = "player_id"

// ---- player identity (stateless cookie, HMAC-signed) ----
//
// The cookie value is "<uuid>-<hmac-hex>": the HMAC-SHA256 of the canonical
// (hyphenated) UUID string under the server secret, truncated to 16 bytes so
// the tag is 32 hex chars. A forged or tampered cookie fails verification and
// the client is silently minted a fresh identity (ideal for a demo: players
// appear anonymous, not broken).

// signPlayer signs a UUID for use as a player_id cookie value.
func signPlayer(secret []byte, id uuid.UUID) string {
	return id.String() + ":" + hex.EncodeToString(hmacTag(secret, id))
}

// verifyPlayer validates a player_id cookie value, returning the UUID and true
// on success. Any malformed or forged value returns (uuid.Nil, false).
func verifyPlayer(secret []byte, raw string) (uuid.UUID, bool) {
	i := strings.LastIndexByte(raw, ':')
	if i < 0 {
		return uuid.Nil, false
	}
	canonical, tagHex := raw[:i], raw[i+1:]
	u, err := uuid.Parse(canonical)
	if err != nil {
		return uuid.Nil, false
	}
	// Recompute over the canonical form so either cookie spelling validates,
	// and compare in constant time to avoid a timing oracle on the tag.
	want := hex.EncodeToString(hmacTag(secret, u))
	if !hmac.Equal([]byte(want), []byte(tagHex)) {
		return uuid.Nil, false
	}
	return u, true
}

func hmacTag(secret []byte, id uuid.UUID) []byte {
	h := hmac.New(sha256.New, secret)
	_, _ = h.Write([]byte(id.String()))
	return h.Sum(nil)[:16]
}

type ctxKey int

const playerKey ctxKey = 0

// playerFromCookie returns the player identity for this request. A missing,
// malformed, or forged cookie is treated as a brand-new player: a fresh UUIDv7
// is minted and the signed cookie is set on the response.
func (s *Server) playerFromCookie(w http.ResponseWriter, r *http.Request) uuid.UUID {
	if c, err := r.Cookie(playerCookie); err == nil {
		if u, ok := verifyPlayer(s.secret, c.Value); ok {
			return u
		}
	}
	u := uuid.Must(uuid.NewV7())
	http.SetCookie(w, &http.Cookie{
		Name:     playerCookie,
		Value:    signPlayer(s.secret, u),
		Path:     "/",
		Expires:  time.Now().Add(365 * 24 * time.Hour),
		Secure:   false, // demo runs on localhost
		HttpOnly: true,  // not readable from JS: defends against XSS impersonation
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
	idx, err := template.New("index.html").ParseFS(indexHTML, "templates/index.html")
	if err != nil {
		return nil, err
	}
	return &templateRenderer{index: idx}, nil
}

func (tr *templateRenderer) renderIndex(w io.Writer, svc *game.Service) error {
	bmpB64, locks := svc.SnapshotBmp()
	gw, gh := svc.Grid()
	initialJSON, _ := json.Marshal(map[string]any{"bmp": bmpB64, "locks": locks})
	minMs, maxMs := svc.TransferTimeRange()
	return tr.index.ExecuteTemplate(w, "index.html", map[string]any{
		"InitialJSON": template.JS(initialJSON), // valid JSON object; trusted (base64 + ints only)
		"Cols":        gw,
		"Rows":        gh,
		"MinTsMs":     minMs,
		"MaxTsMs":     maxMs,
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
