// PixelBeetle game server: SSR pages + claim endpoints + SSE hub.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"pixelbeetle/internal/game"
	"pixelbeetle/internal/hub"
	"pixelbeetle/internal/replay"
	"pixelbeetle/internal/tbclient"
	"pixelbeetle/internal/web"
)

func main() {
	var (
		addr        = flag.String("addr", ":8080", "http listen address")
		tbAddr      = flag.String("tb-addresses", "127.0.0.1:3000", "comma-separated TigerBeetle replica addresses")
		clusterID   = flag.Uint64("cluster-id", 0, "TigerBeetle cluster id")
		grid        = flag.String("grid", "256x256", "canvas size as WxH (1000x1000 for the 1M-pixel demo)")
		logLevel    = flag.String("log", "info", "log level (debug|info|warn|error)")
		reapPeriod  = flag.Duration("reap-period", time.Second, "lock reaper interval")
		warmup      = flag.Bool("warmup", true, "rebuild pixel cache from TigerBeetle transfer history at startup")
		eager       = flag.Bool("eager", true, "create+fund all pixel accounts at startup (N accounts before the first paint)")
		snapshot    = flag.String("snapshot", "data/snapshot.bin", "on-disk materialized-state snapshot path (\"\" = full replay every boot)")
		snapEvery   = flag.Duration("snapshot-every", 30*time.Second, "periodic snapshot interval; only writes when history grew")
		cdcURL      = flag.String("cdc-url", "", "AMQP URL for the TigerBeetle CDC stream (empty = disabled), e.g. amqp://guest:guest@localhost:5672/")
		cdcExchange = flag.String("cdc-exchange", "tigerbeetle", "AMQP exchange the CDC job publishes to")
		secret      = flag.String("secret", "pixelbeetle-demo-secret-change-me", "HMAC key signing the player_id cookie")
	)
	flag.Parse()

	level := slog.LevelInfo
	switch strings.ToLower(*logLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	// Everything below — CDC, reaper, snapshot ticker, metrics, HTTP — belongs
	// to one process lifetime. Ctrl-C / SIGTERM cancels it and each component
	// drains instead of being killed mid-write.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var gw, gh uint32
	if _, err := fmt.Sscanf(*grid, "%dx%d", &gw, &gh); err != nil {
		log.Error("invalid -grid, want WxH like 64x64", "got", *grid)
		os.Exit(1)
	}

	client, err := tbclient.Connect(*clusterID, strings.Split(*tbAddr, ","))
	if err != nil {
		log.Error("connect tigerbeetle", "err", err)
		os.Exit(1)
	}
	defer client.Close()

	h := hub.New(log)
	svc := game.New(gw, gh, client, h, log)

	if *eager {
		if err := svc.InitAllPixels(); err != nil {
			log.Error("eager pixel init", "err", err)
			os.Exit(1)
		}
	}

	if *snapshot != "" {
		svc.SetSnapshot(*snapshot)
	}

	if *warmup {
		if err := svc.WarmCache(); err != nil {
			log.Error("warm cache", "err", err)
			os.Exit(1)
		}
	}

	go func() { // unlock stale UI locks; TB auto-expires the transfers themselves
		t := time.NewTicker(*reapPeriod)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				svc.ReapExpired()
			}
		}
	}()

	if *cdcURL != "" { // live CDC subscriber keeps the cache in sync; reconnects internally
		consumer := replay.NewConsumer(replay.Config{
			AMQPURL:  *cdcURL,
			Exchange: *cdcExchange,
			Log:      log,
		}, svc)
		go func() {
			log.Info("CDC consumer starting", "url", *cdcURL, "exchange", *cdcExchange)
			if err := consumer.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Error("CDC consumer stopped", "err", err)
			}
		}()
	}

	webSrv, err := web.New(svc, h, log, *secret)
	if err != nil {
		log.Error("build web server", "err", err)
		os.Exit(1)
	}
	webSrv.StartMetrics(ctx) // live dashboard over the SSE hub; stops on shutdown

	if *snapshot != "" && *warmup && *snapEvery > 0 { // persist the derived state so the next boot is O(delta)
		// Gated on -warmup: a server that skipped warmup holds partial state
		// (CDC-fed only) and must never write the snapshot file. SaveSnapshot
		// enforces the same invariant defensively.
		// Baseline: write once right after warmup so the boot-time replay work is
		// persisted even if the process dies before the next tick. Then the ticker
		// only rewrites when the watermark has advanced (any ledger activity or
		// server-originated confirm), avoiding pointless rewrites.
		if err := svc.SaveSnapshot(*snapshot); err != nil {
			log.Error("snapshot save (initial)", "err", err)
		}
		last := svc.WarmTs()
		go func() {
			t := time.NewTicker(*snapEvery)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					cur := svc.WarmTs()
					if cur == last {
						continue // nothing new to snapshot
					}
					if err := svc.SaveSnapshot(*snapshot); err != nil {
						log.Error("snapshot save", "err", err)
					} else {
						last = cur
					}
				}
			}
		}()
	}

	// No WriteTimeout: the SSE stream is long-lived and would be killed by a
	// fixed write deadline. ReadHeader/Idle still bound idle connections.
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           webSrv.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Info("pixelbeetle starting", "addr", *addr, "desc", svc.Describe())
	srvErr := make(chan error, 1)
	go func() { srvErr <- httpSrv.ListenAndServe() }()

	select {
	case <-ctx.Done(): // SIGINT/SIGTERM: drain and exit cleanly
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			log.Warn("graceful shutdown", "err", err)
		}
	case err := <-srvErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("listen", "err", err)
			os.Exit(1)
		}
	}
}
