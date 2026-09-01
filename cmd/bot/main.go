// PixelBeetle load generator entry point.
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

// multiFlag is a repeatable string flag (-draw can appear multiple times).
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

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
		paint    = flag.String("paint", "", "blueprint to paint: .txt art file, or image .png/.jpg/.jpeg/.gif (enables paint mode; see draw-plan.md)")
		preview  = flag.String("preview", "", "blueprint to PREVIEW (same formats as -paint): fetch current canvas, overlay, print ASCII, place no claims")
		paintOff = flag.String("paint-offset", "", "x,y top-left anchor for the drawing (default: centered)")
		paintWrk = flag.Int("paint-workers", 16, "parallel paint workers")
		paintOrd = flag.String("paint-order", "scanline", "paint order: scanline | random")
		paintSz  = flag.String("paint-size", "", "max WxH for image→blueprint conversion (default: fit the grid, aspect-preserving)")
		inspect  = flag.Bool("inspect", false, "print the blueprint as text art (legend + rows) and exit — no painting")
		metricsA = flag.String("metrics-addr", ":9090", "metrics listen address; empty disables")
		logLevel = flag.String("log", "info", "log level")
	)
	var draws multiFlag
	flag.Var(&draws, "draw", "shape spec, repeatable: rect/fillrect x,y,w,h,#hex | circle cx,cy,r,#hex | line x0,y0,x1,y1,#hex | text x,y,String,#hex")
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
	if _, err := fmt.Sscanf(*grid, "%dx%d", &cfg.GridW, &cfg.GridH); err != nil {
		log.Error("invalid -grid, want WxH like 256x256", "got", *grid)
		os.Exit(1)
	}
	if *tbAddr != "" {
		cfg.TBAddrs = strings.Split(*tbAddr, ",")
		cfg.Cluster = *cluster
	}
	if *hotspot != "" {
		var x, y uint32
		if _, err := fmt.Sscanf(*hotspot, "%d:%d", &x, &y); err != nil {
			log.Error("invalid -hotspot, want x:y", "got", *hotspot)
			os.Exit(1)
		}
		if x >= cfg.GridW || y >= cfg.GridH {
			log.Error("hotspot out of bounds", "x", x, "y", y, "grid", fmt.Sprintf("%dx%d", cfg.GridW, cfg.GridH))
			os.Exit(1)
		}
		cfg.Hotspot = [2]uint32{x, y}
	}
	// flagWasSet reports whether the named flag appeared on the command line.
	// The painter needs this to distinguish "user said -duration=1m" (a cap)
	// from the load-mode default it must suppress.
	flagWasSet := func(name string) bool {
		set := false
		flag.Visit(func(f *flag.Flag) {
			if f.Name == name {
				set = true
			}
		})
		return set
	}

	// -paint and -preview are different modes over the same loader.
	if *preview != "" && *paint != "" {
		log.Error("-paint and -preview are mutually exclusive")
		os.Exit(1)
	}

	if *paint != "" || *preview != "" || len(draws) > 0 {
		cfg.BlueprintPath = *preview
		if *paint != "" {
			cfg.BlueprintPath = *paint
		}
		cfg.Draws = draws
		cfg.PaintWorkers = *paintWrk
		cfg.PaintOrder = *paintOrd
		if cfg.PaintOrder != "scanline" && cfg.PaintOrder != "random" {
			log.Error("invalid -paint-order, want scanline|random", "got", cfg.PaintOrder)
			os.Exit(1)
		}
		if *paintSz != "" {
			var w, h uint32
			if _, err := fmt.Sscanf(*paintSz, "%dx%d", &w, &h); err != nil || w == 0 || h == 0 {
				log.Error("invalid -paint-size, want WxH like 100x100", "got", *paintSz)
				os.Exit(1)
			}
			cfg.PaintSize = [2]uint32{w, h}
			cfg.PaintSizeSet = true
		}
		if *paintOff != "" {
			var x, y uint32
			if _, err := fmt.Sscanf(*paintOff, "%d,%d", &x, &y); err != nil {
				log.Error("invalid -paint-offset, want x,y", "got", *paintOff)
				os.Exit(1)
			}
			cfg.PaintOffset = [2]uint32{x, y}
			cfg.PaintOffsetSet = true
		}
		// The default -duration/-ramp are load-mode artifacts: paint mode runs
		// until the blueprint finishes unless the user explicitly sets them.
		if !flagWasSet("duration") {
			cfg.Duration = 0
		}
		if !flagWasSet("ramp") {
			cfg.Ramp = 0
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *metricsA != "" {
		go func() { _ = http.ListenAndServe(*metricsA, nil) }() // pprof + future /metrics
	}

	// -inspect: compile the paint input and print its text-art form. No
	// server, no TigerBeetle — just the loader + formatter.
	if *inspect {
		if cfg.BlueprintPath == "" && len(cfg.Draws) == 0 {
			log.Error("-inspect needs -paint or -draw")
			os.Exit(1)
		}
		bp, err := bot.LoadPaint(cfg)
		if err != nil {
			log.Error("inspect failed", "err", err)
			os.Exit(1)
		}
		fmt.Print(bot.FormatTextArt(bp))
		return
	}

	// -preview: fetch the live canvas and show placement/collisions. Same
	// loader and offset rules as painting, so what you see is what will paint.
	if *preview != "" {
		if err := bot.Preview(ctx, cfg, log); err != nil {
			log.Error("preview failed", "err", err)
			os.Exit(1)
		}
		return
	}

	m, err := bot.Run(ctx, cfg, log)
	if err != nil {
		log.Error("bot run failed", "err", err)
		os.Exit(1)
	}
	if *paint != "" || len(draws) > 0 {
		log.Info("final report",
			"painted", m.Painted.Load(), "total", m.Total,
			"lock_conflicts", m.LockConflicts.Load(), "errors", m.Errors.Load(),
		)
		return
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
