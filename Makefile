COMPOSE := docker compose -f deploy/docker-compose.yml
BINS := tollgate-gw mockbackend

.PHONY: build test race vet fmt-check up down logs chaos-kill-backend chaos-kill-backend-local

build:
	@mkdir -p bin
	@for b in $(BINS); do go build -o bin/$$b ./cmd/$$b || exit 1; done

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt-check:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)

up:
	$(COMPOSE) up -d --build

down:
	$(COMPOSE) down -v

logs:
	$(COMPOSE) logs -f

# M1 acceptance: kill a backend and measure how long until traffic shifts.
chaos-kill-backend:
	MODE=docker bench/chaos/kill-backend.sh

chaos-kill-backend-local: build
	MODE=local bench/chaos/kill-backend.sh
