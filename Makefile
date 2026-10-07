# Close Copilot — developer entry points (CC-101).
# Values in .env (copied from .env.example) are exported to every command.
-include .env
export

GO        ?= go
PKGS      := ./...
BIN       := bin
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -X github.com/abhishekjha/close-copilot/internal/buildinfo.Version=$(VERSION)
COMPOSE   := docker compose -f deploy/docker-compose.yml
ERP_COMPOSE := deploy/erpnext/docker-compose.yaml
SUITE     ?= suite-skeleton
GOOSE     := $(GO) run github.com/pressly/goose/v3/cmd/goose@v3.28.0

.DEFAULT_GOAL := help

.PHONY: help
help: ## List targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-12s %s\n", $$1, $$2}'

## ---- Go ----------------------------------------------------------------

.PHONY: deps
deps: ## Resolve dependencies and write go.sum (run once after cloning)
	$(GO) mod tidy
	$(GO) mod verify

.PHONY: build
build: ## Compile every package and the binaries into bin/
	$(GO) build $(PKGS)
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/ ./cmd/...

.PHONY: vet
vet: ## go vet
	$(GO) vet $(PKGS)

.PHONY: lint
lint: ## golangci-lint (install: https://golangci-lint.run/welcome/install/)
	golangci-lint run

.PHONY: test
test: ## Unit tests with the race detector
	$(GO) test -race -count=1 $(PKGS)

.PHONY: test-int
test-int: ## Integration tests (need Docker for testcontainers)
	$(GO) test -race -count=1 -tags=integration $(PKGS)

.PHONY: tidy-check
tidy-check: ## Fail if go.mod or go.sum need tidying
	$(GO) mod tidy -diff

.PHONY: vuln
vuln: ## govulncheck
	$(GO) tool govulncheck $(PKGS)

.PHONY: generate
generate: ## templ generate (UI templates, CC-1001)
	$(GO) tool templ generate

.PHONY: check
check: build vet lint test ## Build, vet, lint and unit tests (G1-G3 core)

.PHONY: gates
gates: tidy-check check vuln ## Everything CI runs on a pull request; /gates adds the spec checks

## ---- Local stack ---------------------------------------------------------

.PHONY: up
up: ## Start Postgres and TEI (and ERPNext once CC-102 adds its compose file)
	$(COMPOSE) up -d
	@if [ -f $(ERP_COMPOSE) ]; then docker compose -f $(ERP_COMPOSE) up -d; else echo "ERPNext compose file not found yet (CC-102)"; fi

.PHONY: down
down: ## Stop the local stack
	$(COMPOSE) --profile ingest down
	@if [ -f $(ERP_COMPOSE) ]; then docker compose -f $(ERP_COMPOSE) down; fi

.PHONY: up-ingest
up-ingest: ## Also start docling-serve (only needed while ingesting)
	$(COMPOSE) --profile ingest up -d

## ---- App -------------------------------------------------------------------

.PHONY: migrate
migrate: ## Apply database migrations (CC-401)
	$(GOOSE) -dir migrations postgres "$(DATABASE_URL)" up

.PHONY: seed
seed: ## Seed ERPNext and the evidence files for SUITE (CC-307)
	$(GO) run ./cmd/seed all --suite $(SUITE)

.PHONY: load
load: ## Load bank and GSTR-2B files into Postgres (CC-402, CC-403)
	$(GO) run ./cmd/load --suite $(SUITE)

.PHONY: ingest
ingest: ## Ingest the document corpus (CC-803)
	$(GO) run ./cmd/ingest

.PHONY: eval
eval: ## Run and score the eval suite (CC-901, CC-902)
	$(GO) run ./cmd/eval run --suite $(SUITE)

.PHONY: run-agent
run-agent: ## Run the agent service (CC-703, CC-1001)
	$(GO) run ./cmd/agent

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BIN)
