.PHONY: help up down logs cdc server bot build test fmt vet tidy rabbit-up rabbit-down

help:
	@echo "make up        - start TigerBeetle x3 + RabbitMQ + CDC job (docker compose)"
	@echo "make down      - stop everything"
	@echo "make cdc       - run the native CDC job (tigerbeetle amqp)"
	@echo "make server    - run the game server against localhost:3000"
	@echo "make bot RPS=100 DURATION=30s - run load generator"
	@echo "make rabbit-up - start dev RabbitMQ + declare the CDC exchange"
	@echo "make rabbit-down - stop dev RabbitMQ"
	@echo "make build test fmt vet tidy"

up:
	docker compose up -d rabbitmq rabbitmq-init tigerbeetle-0 tigerbeetle-1 tigerbeetle-2
	docker compose up -d cdc

down:
	docker compose down

logs:
	docker compose logs -f --tail=100

cdc:
	./bin/tigerbeetle amqp --addresses=127.0.0.1:3000,127.0.0.1:3001,127.0.0.1:3002 --cluster=0 --host=127.0.0.1 --vhost=/ --user=guest --password=guest --publish-exchange=tigerbeetle

rabbit-up:
	./scripts/dev-rabbit.sh start

rabbit-down:
	./scripts/dev-rabbit.sh stop

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
