// Package bot is the Canvas Clash load generator (plan.md §4).
//
// Modes:
//   - api:    HTTP against the game server (end-to-end: locks, cache, SSE)
//   - direct: straight into TigerBeetle via the shared tbclient wrapper
//     (raw DB ceiling, no app overhead)
package bot

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	tb "github.com/tigerbeetle/tigerbeetle-go"

	"pixelbeetle/internal/tbclient"
)

type Mode string

const (
	ModeAPI    Mode = "api"
	ModeDirect Mode = "direct"
)

type Config struct {
	Target   string        // game server base URL (api mode)
	TBAddrs  []string      // tigerbeetle addresses (direct mode)
	Cluster  uint64        // tigerbeetle cluster id (direct mode)
	RPS      int           // target claims/sec
	Duration time.Duration // total run; 0 = until context cancel
	Ramp     time.Duration // linear ramp-up window
	Players  int           // number of simulated players
	Hotspot  [2]uint32     // if set, all agents fight over this pixel
	Abandon  float64       // fraction of claims never confirmed (~0.10 default)
	ThinkMin time.Duration // min confirm delay
	ThinkMax time.Duration // max confirm delay
}

type Metrics struct {
	ClaimsStarted atomic.Uint64
	Confirmed     atomic.Uint64
	Voided        atomic.Uint64
	LockConflicts atomic.Uint64
	Errors        atomic.Uint64
	latencies     []float64
	latMu         sync.Mutex
}

// Run drives the whole swarm and blocks until Duration elapses or ctx cancels.
func Run(ctx context.Context, cfg Config, log *slog.Logger) (*Metrics, error) {
	if cfg.Abandon <= 0 {
		cfg.Abandon = 0.10
	}
	if cfg.ThinkMin == 0 {
		cfg.ThinkMin = 100 * time.Millisecond
	}
	if cfg.ThinkMax == 0 {
		cfg.ThinkMax = 500 * time.Millisecond
	}

	m := &Metrics{}

	var (
		direct *tbclient.Client
		err    error
	)
	if cfg.Mode() == ModeDirect {
		direct, err = tbclient.Connect(cfg.Cluster, cfg.TBAddrs)
		if err != nil {
			return nil, fmt.Errorf("bot: connect tigerbeetle: %w", err)
		}
		defer direct.Close()
	}

	httpc := &http.Client{Timeout: 5 * time.Second}
	playerIDs := make([]uuid.UUID, cfg.Players)
	for i := range playerIDs {
		playerIDs[i] = uuid.Must(uuid.NewV7())
	}

	ctx, cancel := context.WithCancel(ctx)
	if cfg.Duration > 0 {
		ctx, cancel = context.WithTimeout(ctx, cfg.Duration)
	}
	defer cancel()

	var wg sync.WaitGroup
	tokens := make(chan struct{}) // global pacing: one claim per token

	go func() { // pacer: global token bucket with linear ramp
		start := time.Now()
		interval := time.Second / time.Duration(max(cfg.RPS, 1))
		next := start
		for {
			time.Sleep(time.Until(next))
			select {
			case <-ctx.Done():
				close(tokens)
				return
			default:
			}
			if cfg.Ramp > 0 {
				frac := time.Since(start).Seconds() / cfg.Ramp.Seconds()
				if frac < 1 && rand.Float64() > frac { // thin out during ramp
					next = next.Add(interval)
					continue
				}
			}
			select {
			case tokens <- struct{}{}:
			case <-ctx.Done():
				close(tokens)
				return
			}
			next = next.Add(interval)
		}
	}()

	for i, player := range playerIDs {
		wg.Add(1)
		go func(player uuid.UUID, seed int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(seed)))
			for range tokens {
				select {
				case <-ctx.Done():
					return
				default:
				}
				m.ClaimsStarted.Add(1)

				x := rng.Uint32() % 64 // TODO: wire real grid dims via config
				y := rng.Uint32() % 64
				if cfg.Hotspot[0] != 0 || cfg.Hotspot[1] != 0 {
					x, y = cfg.Hotspot[0], cfg.Hotspot[1]
				}
				color := uint8(rng.Intn(16))

				started := time.Now()
				claimID, err := submitClaim(ctx, httpc, direct, cfg.Target, player, x, y, color)
				if err != nil {
					m.recordFailure(err, &m.Errors)
					continue
				}

				think := cfg.ThinkMin + time.Duration(rng.Int63n(int64(cfg.ThinkMax-cfg.ThinkMin)))
				select {
				case <-time.After(think):
				case <-ctx.Done():
					return
				}

				if rng.Float64() < cfg.Abandon {
					// Walk away: TB auto-expires the pending transfer and CDC
					// emits two_phase_expired. This is the expiry-path demo.
					m.dropLatency(started)
					continue
				}
				if err := resolveClaim(ctx, httpc, direct, cfg.Target, player, claimID, true); err != nil {
					m.recordFailure(err, &m.Errors)
					continue
				}
				m.Confirmed.Add(1)
				m.recordLatency(started)
			}
		}(player, i)
	}

	wg.Wait()

	log.Info("bot run finished",
		"claims", m.ClaimsStarted.Load(),
		"confirmed", m.Confirmed.Load(),
		"conflicts", m.LockConflicts.Load(),
		"errors", m.Errors.Load(),
	)
	return m, nil
}

func (c Config) Mode() Mode {
	if len(c.TBAddrs) > 0 {
		return ModeDirect
	}
	return ModeAPI
}

func (m *Metrics) recordFailure(err error, counter *atomic.Uint64) {
	if err.Error() == "pixel locked by another player" {
		m.LockConflicts.Add(1)
		return
	}
	counter.Add(1)
}

func (m *Metrics) recordLatency(started time.Time) {
	ms := time.Since(started).Seconds() * 1000
	m.latMu.Lock()
	m.latencies = append(m.latencies, ms)
	m.latMu.Unlock()
}

func (m *Metrics) dropLatency(started time.Time) {} // abandoned claims excluded from latency

// LatencyReport returns p50/p99 in ms over recorded full claim cycles.
func (m *Metrics) LatencyReport() (p50, p99 float64) {
	m.latMu.Lock()
	defer m.latMu.Unlock()
	if len(m.latencies) == 0 {
		return 0, 0
	}
	sorted := append([]float64(nil), m.latencies...)
	sort.Float64s(sorted)
	pick := func(q float64) float64 {
		i := int(q * float64(len(sorted)-1))
		return sorted[i]
	}
	return pick(0.50), pick(0.99)
}

// ---- transport adapters ----

func submitClaim(ctx context.Context, hc *http.Client, direct *tbclient.Client, target string, player uuid.UUID, x, y uint32, color uint8) (string, error) {
	if direct != nil { // direct mode: same claim builder, straight to TB
		t := tbclient.NewClaim(x, y, color, player)
		if err := direct.Submit(t); err != nil {
			return "", err
		}
		id := t.ID.Bytes()
		return hex.EncodeToString(id[:]), nil
	}

	body, _ := json.Marshal(map[string]any{"x": x, "y": y, "color": color})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target+"/claim", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "player_id", Value: player.String()})
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusConflict {
		return "", fmt.Errorf("pixel locked by another player")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("claim: %d %s", resp.StatusCode, raw)
	}
	var out struct{ ClaimID string }
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	return out.ClaimID, nil
}

func resolveClaim(ctx context.Context, hc *http.Client, direct *tbclient.Client, target string, player uuid.UUID, claimID string, confirm bool) error {
	if direct != nil {
		_ = player
		raw, err := hex.DecodeString(claimID)
		if err != nil || len(raw) != 16 {
			return fmt.Errorf("invalid claim id %q", claimID)
		}
		var id [16]byte
		copy(id[:], raw)
		return direct.Submit(tbclient.BuildPost(tb.BytesToUint128(id), 0)) // color 0: pending used same code|0
	}
	action := "/cancel"
	if confirm {
		action = "/confirm"
	}
	body, _ := json.Marshal(map[string]any{"claimId": claimID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target+action, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "player_id", Value: player.String()})
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %d %s", action, resp.StatusCode, raw)
	}
	return nil
}

func toTB(b [16]byte) tb.Uint128 { return tb.BytesToUint128(b) }

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
