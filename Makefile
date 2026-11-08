.PHONY: dev test test-unit lint bench migrate migrate-status migration bootstrap gen up down

# .env (cp .env.example .env) configures the targets that start Hardcount or
# its tools: dev, migrate, migrate-status and bootstrap. Only those load it,
# so the test targets see the same environment CI does.
ifneq ($(filter dev migrate migrate-status bootstrap,$(MAKECMDGOALS)),)
-include .env
export
endif

# Run the server from source
dev:
	go run ./cmd/hardcount

# Run all tests.
#
# `-p 1` runs packages sequentially. Integration tests share a single
# test database; running packages in parallel races each package's
# SetupTestDB truncate against another package's in-flight queries
# and produces flaky e2e failures (visible specifically on
# internal/api e2e tests).
test:
	go test -p 1 ./... -count=1

# Run the unit tests only (-short skips the ones that need Postgres)
test-unit:
	go test ./... -short -count=1

# Run linter. Hardcount code rules live in .golangci.yml (forbidigo: wall-clock
# time in clock-pinned packages, single-invoice period lookups) and in Go tests
# under internal/arch (civil-day dates need an explicit zone).
lint:
	golangci-lint run ./...

# Run benchmarks
bench:
	go test ./internal/domain/ -bench=. -benchmem -count=1

# Migration management
migrate:
	go run ./cmd/hardcount migrate

migrate-status:
	go run ./cmd/hardcount migrate status

# Write the next migration file in internal/platform/migrate/sql/, numbered
# after the highest one in any worktree or on any branch. Rules and steps:
# docs/dev/migrations.md.
#   make migration name=add_invoice_purchase_order
#   make migration name=idx_invoices_purchase_order concurrent=1   (an index build)
migration:
	@scripts/new-migration.sh "$(name)" "$(concurrent)"

# Bootstrap: create demo tenant + API keys. DATABASE_URL comes from .env.
bootstrap:
	go run ./cmd/hardcount-bootstrap

# Code generation from the OpenAPI spec at api/openapi.yaml: see
# docs/dev/openapi-workflow.md. It writes the dashboard's typed
# react-query hooks and schema types (orval) into web-v2/src/lib/gen/,
# from a copy of the spec at web-v2/public/openapi.yaml (not committed).
# CI runs `make gen`, then `git diff --exit-code`, so a spec change
# without the regenerated files fails the build. Run it after editing
# the spec, and commit the regenerated files with the spec change.
gen:
	cd web-v2 && npm run gen

# Docker compose for local dev
up:
	docker compose up -d

down:
	docker compose down
