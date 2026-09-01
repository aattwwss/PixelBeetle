.PHONY: help up down reset serve bot db-up db-down broker-up broker-down cdc-up cdc-down build test fmt vet tidy

help:
	@echo "make up        - start dependencies: TigerBeetle cluster (db) + RabbitMQ (broker) + CDC job"
	@echo "make serve     - run the game server in the foreground (:8080, 256x256 by default)"
	@echo "make down      - stop everything"
	@echo "make reset     - stop everything and wipe data/ — clean slate (fresh TB format, empty canvas)"
	@echo "make bot RPS=100 DURATION=30s [PLAYERS=64] [GRID=256x256] - run load generator"
	@echo "make serve GRID=1000x1000     - 1M-pixel showcase (provisions 1M accounts at startup)"
	@echo "make bot   GRID=1000x1000      - load gen against a 1M canvas (must match the server)"
	@echo "make db-up / db-down          - just the TigerBeetle 3-replica cluster"
	@echo "make broker-up / broker-down  - just RabbitMQ + its exchange/sink topology"
	@echo "make cdc-up / cdc-down        - just the supervised tigerbeetle amqp job"
	@echo "make build test fmt vet tidy"

## -- dependencies -----------------------------------------------------------

up: db-up broker-up cdc-up

down:
	-./scripts/cdc.sh stop
	-./scripts/rabbitmq.sh stop
	-./scripts/tigerbeetle.sh stop

db-up:
	./scripts/tigerbeetle.sh start

db-down:
	./scripts/tigerbeetle.sh stop

broker-up:
	./scripts/rabbitmq.sh start

broker-down:
	./scripts/rabbitmq.sh stop

cdc-up:
	./scripts/cdc.sh start

cdc-down:
	./scripts/cdc.sh stop

## -- application -------------------------------------------------------------

# Foreground dev server: warm-up rebuilds the pixel cache from TB history,
# and -cdc-url keeps the cache live-synced off the CDC stream. GRID defaults to
# 256x256; pass GRID=1000x1000 for the 1M-pixel showcase (-eager provisions
# all accounts at startup, idempotent across restarts).
serve:
	go run ./cmd/server \
	  -tb-addresses 127.0.0.1:3000,127.0.0.1:3001,127.0.0.1:3002 \
	  -addr :8080 \
	  -grid $(or $(GRID),256x256) \
	  -cdc-url amqp://guest:guest@127.0.0.1:5672/ \
	  -cdc-exchange tigerbeetle

# Load generator. GRID must match the running server. Defaults match `make serve`.
bot:
	go run ./cmd/bot -target $(or $(TARGET),http://localhost:8080) \
	  -grid $(or $(GRID),256x256) \
	  -rps $(or $(RPS),100) -duration $(or $(DURATION),30s) -players $(or $(PLAYERS),64)

reset: ## stop everything and wipe all game state (TB ledger, snapshots, test leftovers, logs)
	-./scripts/server.sh stop
	-./scripts/cdc.sh stop
	-./scripts/rabbitmq.sh stop
	-./scripts/tigerbeetle.sh stop
	rm -f data/dev_*.tigerbeetle data/snapshot.bin data/snapshot.bin.anchors
	rm -rf data/live-* data/logs/*
	@echo "data/ wiped — 'make up' reformats a fresh cluster; then 'scripts/server.sh start'"

build:
	go build ./...

test:
	go test ./...

fmt:
	gofmt -l -w .

vet:
	go vet ./...

tidy:
	go mod tidy
