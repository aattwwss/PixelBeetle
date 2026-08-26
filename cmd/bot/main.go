// Canvas Clash load generator entry point.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"pixelbeetle/internal/bot"
)

func main() {
	var (
		target   = flag.String("target", "http://localhost:8080", "game server base URL (api mode)")
		tbAddr   = flag.String("tb-addresses", "", "comma-separated TB addresses; enables direct mode")
		cluster  = flag.Uint64("cluster-id", 0, "TigerBeetle cluster id")
		grid     = flag.String("grid", "256x256", "canvas size as WxH (must match the game server)")
		rps      = flag.Int("rps", 100, "target claims/sec")
		duration = flag.Duration("duration", 30*1e9, "total run duration")
		ramp     = flag.Duration("ramp", 5*1e9, "linear ramp-up window")
		players  = flag.Int("players", 64, "simulated player count")
		hotspot  = flag.String("hotspot", "", "x:y — all agents fight over this pixel")
		metricsA = flag.String("metrics-addr", ":9090", "metrics listen address; empty disables")
		logLevel = flag.String("log", "info", "log level")
	)
	flag.Parse()

	level := slog.LevelInfo
	switch strings.ToLower(*logLevel) {
	case "debug":
		level = slog.LevelDebug
	case "error":
		level = slog.LevelError
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg := bot.Config{
		Target:   *target,
		RPS:      *rps,
		Duration: *duration,
		Ramp:     *ramp,
		Players:  *players,
	}
	if _, err := fmtSscanGrid(*grid, &cfg.GridW, &cfg.GridH); err != nil {
		log.Error("invalid -grid, want WxH like 256x256", "got", *grid)
		os.Exit(1)
	}
	if *tbAddr != "" {
		cfg.TBAddrs = strings.Split(*tbAddr, ",")
		cfg.Cluster = *cluster
	}
	if *hotspot != "" {
		var x, y uint32
		if _, err := fmtSscan(*hotspot, &x, &y); err != nil {
			log.Error("invalid -hotspot, want x:y", "got", *hotspot)
			os.Exit(1)
		}
		if x >= cfg.GridW || y >= cfg.GridH {
			log.Error("hotspot out of bounds", "x", x, "y", y, "grid", fmt.Sprintf("%dx%d", cfg.GridW, cfg.GridH))
			os.Exit(1)
		}
		cfg.Hotspot = [2]uint32{x, y}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *metricsA != "" {
		go func() { _ = http.ListenAndServe(*metricsA, nil) }() // pprof + future /metrics
	}

	m, err := bot.Run(ctx, cfg, log)
	if err != nil {
		log.Error("bot run failed", "err", err)
		os.Exit(1)
	}
	p50, p99 := m.LatencyReport()
	log.Info("final report",
		"claims_started", m.ClaimsStarted.Load(),
		"confirmed", m.Confirmed.Load(),
		"lock_conflicts", m.LockConflicts.Load(),
		"errors", m.Errors.Load(),
		"p50_ms", p50,
		"p99_ms", p99,
	)
}
