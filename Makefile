SHELL := /bin/bash

POSTGRES_SUPERUSER_URL ?= postgres://postgres:postgres@localhost:5432/paybook?sslmode=disable
TEST_DATABASE_URL ?= postgres://paybook_app:paybook_app_pw@localhost:5432/paybook?sslmode=disable

.PHONY: help
help:
	@awk 'BEGIN {FS = ":.*?##"} /^[a-zA-Z_-]+:.*?##/ {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: up
up: ## Start postgres + app + prometheus via docker compose
	docker compose up -d --build
	@./scripts/wait-for-db.sh
	@$(MAKE) migrate
	@$(MAKE) seed
	@echo "ready. try: ./scripts/demo.sh"

.PHONY: down
down: ## Stop and remove compose stack
	docker compose down -v

.PHONY: migrate
migrate: ## Apply schema migrations against POSTGRES_SUPERUSER_URL
	@psql "$(POSTGRES_SUPERUSER_URL)" -v ON_ERROR_STOP=1 -f migrations/0001_initial.sql

.PHONY: seed
seed: ## Seed customers, deployments and virtual accounts for demo/load tests
	@psql "$(POSTGRES_SUPERUSER_URL)" -v ON_ERROR_STOP=1 -f scripts/seed.sql

.PHONY: build
build: ## Build the api binary into bin/api
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/api ./cmd/api

.PHONY: run
run: ## Run the api from source (expects .env loaded)
	go run ./cmd/api

.PHONY: test
test: ## Run unit + integration + property tests with race detector
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test -race -count=1 -timeout=5m ./...

.PHONY: test-unit
test-unit: ## Unit tests only (no db required)
	go test -race -count=1 ./internal/...

.PHONY: property
property: ## Property tests, longer run
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test -race -count=1 -timeout=10m -run=TestProperty ./test/property/...

.PHONY: load-steady
load-steady: ## k6 steady-state at 2k TPS for 60s
	k6 run --summary-export=load/results/steady.json load/steady.js

.PHONY: load-burst
load-burst: ## k6 replay-storm burst scenario
	k6 run --summary-export=load/results/burst.json load/burst.js

.PHONY: chaos-db-kill
chaos-db-kill: ## Kill postgres mid-load, verify recovery
	./test/chaos/db_kill.sh

.PHONY: chaos-clock-skew
chaos-clock-skew: ## Submit payments with future transaction_date, expect reject
	./test/chaos/clock_skew.sh

.PHONY: chaos-network-partition
chaos-network-partition: ## Partition app from db, verify 503s without corruption
	./test/chaos/network_partition.sh

.PHONY: demo
demo: ## End-to-end demo against a running stack
	./scripts/demo.sh

.PHONY: lint
lint: ## Run go vet and staticcheck (if installed)
	go vet ./...
	@if command -v staticcheck >/dev/null 2>&1; then staticcheck ./...; else echo "staticcheck not installed, skipping"; fi

.PHONY: tidy
tidy: ## go mod tidy
	go mod tidy
