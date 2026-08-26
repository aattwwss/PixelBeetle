.PHONY: help up down serve bot rabbit-up rabbit-down tb-up tb-down cdc build test fmt vet tidy

help:
	@echo "make up        - start dependencies: TigerBeetle x3 + RabbitMQ + CDC job"
	@echo "make serve     - run the game server in the foreground (:8080)"
	@echo "make down      - stop everything (server included if via run-server.sh)"
	@echo "make bot RPS=100 DURATION=30s [PLAYERS=64] - run load generator"
	@echo "make tb-up / tb-down      - just the TigerBeetle cluster"
	@echo "make rabbit-up / rabbit-down - just RabbitMQ + exchange + sink queue"
	@echo "make build test fmt vet tidy"

## -- dependencies -----------------------------------------------------------

up: tb-up rabbit-up
	./scripts/run-cdc.sh start

down:
	-./scripts/run-cdc.sh stop
	-./scripts/dev-rabbit.sh stop
	-./scripts/dev-cluster.sh stop

tb-up:
	./scripts/dev-cluster.sh start

tb-down:
	./scripts/dev-cluster.sh stop

rabbit-up:
	./scripts/dev-rabbit.sh start

rabbit-down:
	./scripts/dev-rabbit.sh stop

cdc:
	./scripts/run-cdc.sh start

## -- application -------------------------------------------------------------

# Foreground dev server: warm-up rebuilds the pixel cache from TB history,
# and -cdc-url keeps the cache live-synced off the CDC stream.
serve:
	go run ./cmd/server \
	  -tb-addresses 127.0.0.1:3000,127.0.0.1:3001,127.0.0.1:3002 \
	  -addr :8080 \
	  -cdc-url amqp://guest:guest@127.0.0.1:5672/ \
	  -cdc-exchange tigerbeetle

bot:
	go run ./cmd/bot -target $(or $(TARGET),http://localhost:8080) \
	  -rps $(or $(RPS),100) -duration $(or $(DURATION),30s) -players $(or $(PLAYERS),64)

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
