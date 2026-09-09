# The canonical spec — read this before building anything.
export SPEC := docs/openlane_spec.md

# Local dev URLs
API_URL ?= http://localhost:8080
WEB_URL ?= http://localhost:3000

.DEFAULT_GOAL := help

.PHONY: help dev api worker web check lint test e2e db-up db-down db-migrate db-reset db-seed demo fmt build

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

dev: ## Run api + worker + web with hot reload (needs docker services up)
	$(MAKE) -j3 api worker web

api: ## Run Go API on :8080
	cd apps/api && go run ./cmd/api

worker: ## Run Go worker (River)
	cd apps/worker && go run ./cmd/worker

web: ## Run Next.js on :3000
	cd apps/web && npm run dev

db-up: ## Start postgres + redis + vaults3
	docker compose up -d postgres redis vaults3

db-down: ## Stop local services
	docker compose down

GOOSE ?= goose

db-migrate: ## Apply all goose migrations
	$(GOOSE) -dir db/migrations postgres "$(DATABASE_URL)" up

db-seed: ## Load the DEMO seed (destroys current demo rows)
	psql "$(DATABASE_URL)" -f db/seed/demo.sql

demo: ## Migrate + seed + print a clickable portal URL
	$(GOOSE) -dir db/migrations postgres "$(DATABASE_URL)" up
	$(MAKE) db-seed
	@echo ""
	@echo "  Portal (open in a browser):"
	@echo "    $(WEB_URL)/p/nAKHcRHg8meLXNaD6Q9sG8V2GkhbUI6pL9FRVb3gAJs"
	@echo ""
	@echo "  API smoke (as the portal token):"
	@echo "    curl -s $(API_URL)/v1/portal/nAKHcRHg8meLXNaD6Q9sG8V2GkhbUI6pL9FRVb3gAJs/session"
	@echo ""
	@echo "  Fresh link (staff; token prints once):"
	@printf '    curl -s -X POST %s/v1/portal-links -H "Authorization: Bearer $$OPENLANE_STAFF_TOKEN" -H "X-Workspace-Id: 11111111-1111-1111-1111-111111111111" -H "Content-Type: application/json" -d \x27{"project_id":"55555555-5555-5555-5555-555555555555","contact_id":"44444444-4444-4444-4444-444444444444"}\x27\n' "$(API_URL)"

db-reset: ## Drop volumes and start clean (DESTROYS local data)
	docker compose down -v && $(MAKE) db-up

check: lint test sqlc-diff ## Everything CI runs

lint: ## golangci-lint
	golangci-lint run ./...

test: ## Go tests (unit + RLS)
	go test ./...

sqlc-diff: ## Fail if generated sqlc code is stale
	cd db && sqlc diff

e2e: ## Playwright suite (needs stack running)
	cd e2e && npx playwright test

fmt: ## gofmt + organize imports
	go fmt ./... && goimports -w ./apps

build: ## Build release binaries
	CGO_ENABLED=0 go build -o bin/api ./apps/api/cmd/api
	CGO_ENABLED=0 go build -o bin/worker ./apps/worker/cmd/worker
