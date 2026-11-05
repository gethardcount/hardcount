.PHONY: build run test lint migrate migration dev clean gen

# .env (cp .env.example .env) configures the targets that start Hardcount or
# its tools: run, dev, migrate, migrate-status and bootstrap. Only those
# load it, so the test targets see the same environment CI does.
ifneq ($(filter run dev migrate migrate-status bootstrap,$(MAKECMDGOALS)),)
-include .env
export
endif

# Build the hardcount binary
build:
	go build -o bin/hardcount ./cmd/hardcount

# Run the server locally
run: build
	./bin/hardcount

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

# Tidy dependencies
tidy:
	go mod tidy

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

# Clean build artifacts
clean:
	rm -rf bin/

# Bootstrap: create demo tenant + API keys
bootstrap:
	DATABASE_URL="postgres://hardcount:hardcount@localhost:5432/hardcount?sslmode=disable" \
	go run ./cmd/hardcount-bootstrap

# Run demo script (requires: make up, make bootstrap, make dev)
demo:
	@echo "Run: ./scripts/demo.sh <YOUR_SECRET_KEY>"

# Code generation from the OpenAPI spec at api/openapi.yaml: see
# docs/dev/openapi-workflow.md. It writes the dashboard's typed
# react-query hooks and schema types (orval) into web-v2/src/lib/gen/,
# from a copy of the spec at web-v2/public/openapi.yaml (not committed).
# CI runs `make gen`, then `git diff --exit-code`, so a spec change
# without the regenerated files fails the build. Run it after editing
# the spec, and commit the regenerated files with the spec change.
gen:
	cd web-v2 && npm run gen

# Frontend
web-install:
	cd web-v2 && npm install

web-dev:
	cd web-v2 && npm run dev

web-build:
	cd web-v2 && npm run build

# Docker compose for local dev
up:
	docker compose up -d

down:
	docker compose down

# Project stats
stats:
	@echo "━━━ Hardcount Project Stats ━━━"
	@echo ""
	@printf "  Go files:     %s\n" "$$(find . -name '*.go' -not -path './vendor/*' | wc -l | tr -d ' ')"
	@printf "  Go lines:     %s\n" "$$(find . -name '*.go' -not -path './vendor/*' -exec cat {} + | wc -l | tr -d ' ')"
	@printf "  Test files:   %s\n" "$$(find . -name '*_test.go' | wc -l | tr -d ' ')"
	@printf "  SQL files:    %s\n" "$$(find . -name '*.sql' | wc -l | tr -d ' ')"
	@printf "  Packages:     %s\n" "$$(find ./internal -type d | wc -l | tr -d ' ')"
	@printf "  API routes:   %s\n" "$$(grep -rhoE '(^|[^A-Za-z0-9_])r\.(Get|Post|Put|Patch|Delete|Method|Handle)\(' internal --include='*.go' --exclude='*_test.go' | wc -l | tr -d ' ')"
	@printf "  TS/TSX files: %s\n" "$$(find ./web-v2/src -name '*.ts' -o -name '*.tsx' 2>/dev/null | wc -l | tr -d ' ')"
	@echo ""
	@printf "  Tests:        " && go test ./... -short -count=1 2>&1 | grep -c "^ok" && echo ""
	@echo "━━━━━━━━━━━━━━━━━━━━━━━━━━"
