COMPOSE := docker compose -f deploy/docker-compose.yml
BINS := tollgate-gw mockbackend

.PHONY: build test race vet fmt-check up down logs chaos-kill-backend chaos-kill-backend-local bench-e1 bench-e2 bench-e7 report fuzz-smoke

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

# Benchmarks: full runs write bench/results/; then `make report`.
bench-e1:
	go run ./bench/run e1

bench-e2:
	go run ./bench/run e2

bench-e7:
	go run ./bench/run e7

report:
	go run ./bench/report.go

# 30-second fuzz smoke run per target (CI runs the same).
FUZZ_TIME ?= 30s
fuzz-smoke:
	go test ./internal/rules/lexer -run '^$$' -fuzz FuzzLexer -fuzztime $(FUZZ_TIME)
	go test ./internal/rules/parser -run '^$$' -fuzz FuzzParser -fuzztime $(FUZZ_TIME)
	go test ./internal/rules -run '^$$' -fuzz FuzzVMBudget -fuzztime $(FUZZ_TIME)
	go test ./internal/rules -run '^$$' -fuzz FuzzDifferential -fuzztime $(FUZZ_TIME)
	go test ./internal/ai/filter -run '^$$' -fuzz FuzzSplitPatterns -fuzztime $(FUZZ_TIME)
