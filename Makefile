.PHONY: help up down logs cdc server bot build test fmt vet tidy

help:
	@echo "make up      - start TigerBeetle x3 + RabbitMQ + CDC job (docker compose)"
	@echo "make down    - stop everything"
	@echo "make cdc     - restart the CDC job only"
	@echo "make server  - run the game server against localhost:3000"
	@echo "make bot RPS=100 DURATION=30s - run load generator"
	@echo "make build test fmt vet tidy"

up:
	docker compose up -d rabbitmq rabbitmq-init tigerbeetle-0 tigerbeetle-1 tigerbeetle-2
	docker compose up -d cdc

down:
	docker compose down

logs:
	docker compose logs -f --tail=100

cdc:
	docker compose up -d --force-recreate cdc

server:
	go run ./cmd/server -tb-addresses 127.0.0.1:3000,127.0.0.1:3001,127.0.0.1:3002

bot:
	go run ./cmd/bot -target $(TARGET) -rps $(or $(RPS),100) -duration $(or $(DURATION),30s) -players $(or $(PLAYERS),64)

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
