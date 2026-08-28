// PixelBeetle game server: SSR pages + claim endpoints + SSE hub.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"strings"
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

	var gw, gh uint32
	if _, err := fmtSscan(*grid, &gw, &gh); err != nil {
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
		for range t.C {
			svc.ReapExpired()
		}
	}()

	if *cdcURL != "" { // live CDC subscriber keeps the cache in sync
		consumer := replay.NewConsumer(replay.Config{
			AMQPURL:  *cdcURL,
			Exchange: *cdcExchange,
			Log:      log,
		}, svc)
		go func() {
			log.Info("CDC consumer starting", "url", *cdcURL, "exchange", *cdcExchange)
			if err := consumer.Run(context.Background()); err != nil && err != context.Canceled {
				log.Error("CDC consumer stopped", "err", err)
			}
		}()
	}

	webSrv, err := web.New(svc, h, log, *secret)
	if err != nil {
		log.Error("build web server", "err", err)
		os.Exit(1)
	}
	webSrv.StartMetrics(context.Background()) // live dashboard over the SSE hub

	if *snapshot != "" && *warmup && *snapEvery > 0 { // persist the derived state so the next boot is O(delta)
		// Gated on -warmup: a server that skipped warmup holds partial state
		// (CDC-fed only) and must never write the snapshot file. SaveSnapshot
		// enforces the same invariant defensively.
		// Baseline: write once right after warmup so the boot-time replay work is
		// persisted even if the process dies before the next tick. Then the ticker
		// only rewrites when history has grown (avoids rewriting a 50MB file
		// pointlessly every interval).
		if err := svc.SaveSnapshot(*snapshot); err != nil {
			log.Error("snapshot save (initial)", "err", err)
		} else {
			log.Info("snapshot saved (initial)", "path", *snapshot,
				"events", svc.HistoryLen())
		}
		last := svc.HistoryLen()
		go func() {
			t := time.NewTicker(*snapEvery)
			defer t.Stop()
			for range t.C {
				cur := svc.HistoryLen()
				if cur == last {
					continue // nothing new to snapshot
				}
				if err := svc.SaveSnapshot(*snapshot); err != nil {
					log.Error("snapshot save", "err", err)
				} else {
					log.Info("snapshot saved", "path", *snapshot, "events", cur)
				}
				last = cur
			}
		}()
	}

	log.Info("pixelbeetle starting", "addr", *addr, "desc", svc.Describe())
	if err := http.ListenAndServe(*addr, webSrv.Routes()); err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
}

// fmtSscan avoids importing fmt just for one call site.
func fmtSscan(s string, a, b *uint32) (int, error) {
	return sscan(s, a, b)
}
