// Canvas Clash game server: SSR pages + claim endpoints + SSE hub.
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
		grid        = flag.String("grid", "64x64", "canvas size as WxH")
		logLevel    = flag.String("log", "info", "log level (debug|info|warn|error)")
		reapPeriod  = flag.Duration("reap-period", time.Second, "lock reaper interval")
		warmup      = flag.Bool("warmup", true, "rebuild pixel cache from TigerBeetle transfer history at startup")
		cdcURL      = flag.String("cdc-url", "", "AMQP URL for the TigerBeetle CDC stream (empty = disabled), e.g. amqp://guest:guest@localhost:5672/")
		cdcExchange = flag.String("cdc-exchange", "tigerbeetle", "AMQP exchange the CDC job publishes to")
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

	webSrv, err := web.New(svc, h, log)
	if err != nil {
		log.Error("build web server", "err", err)
		os.Exit(1)
	}

	log.Info("canvas clash starting", "addr", *addr, "desc", svc.Describe())
	if err := http.ListenAndServe(*addr, webSrv.Routes()); err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
}

// fmtSscan avoids importing fmt just for one call site.
func fmtSscan(s string, a, b *uint32) (int, error) {
	return sscan(s, a, b)
}
