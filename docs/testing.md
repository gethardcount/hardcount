# How we test Velox

This page is for anyone changing Velox. It says which test to write for a
change, how to run each kind of test, and what is checked by hand before a
release. Every change ships with a test at the right layer.

## The four layers

| Layer | Tools | What belongs here |
|---|---|---|
| 1. Logic and money | `go test`: unit tests (`-short`) and integration tests against a real Postgres (`internal/testutil`) | All money math, state changes, Row-Level Security (RLS), migrations, API contracts and architecture rules (the source tests in `internal/arch`) |
| 2. Static checks | golangci-lint, eslint, `tsc -b`, codegen drift (`make gen`) | Code rules. A rule we can check by reading the code becomes a linter rule or a Go test, not a script |
| 3. Dashboard end to end | Playwright (`@playwright/test`) | A smoke test over every page, plus a few key user journeys, against a real API with seeded data |
| 4. Live services | [Release checklist](ops/release-checklist.md), run by hand | Real Stripe test mode, real email, the LiteLLM callback and the invoice PDF. Run before each release tag |

Most tests belong in layer 1. Layers 3 and 4 stay small on purpose.

## Which test to write

| You changed | Write |
|---|---|
| Money, an invoice, payment, credit, dunning, subscription or tax state | A Go integration test against real Postgres. Follow the [money-path playbook](dev/money-path-robustness-playbook.md) §5: test the collision, not only the happy path |
| Pure logic with no database (a calculation, a parser, a formatter) | A Go unit test, or a Vitest test for dashboard code in `web-v2/src/lib` |
| An API endpoint | A Go test that calls the handler and checks the status code and body |
| A rule about how code may be written | A golangci-lint rule in `.golangci.yml`, or a source test in `internal/arch` |
| A dashboard page or a user journey | A Playwright spec in `web-v2/e2e/`. New pages are added to the smoke list |
| Stripe, email or PDF behavior that a fake cannot prove | A line in the [release checklist](ops/release-checklist.md), plus a Go test for the logic around it |

A Playwright test checks that the screen shows what the API returned.
Arithmetic stays in Go tests.

## Run each layer locally

Start Postgres once: `docker compose up -d postgres`.

```bash
# Layer 1: Go
go test ./... -short                 # unit tests, no database
go test -p 1 ./... -short=false      # unit + integration; test databases are created for you

# Layer 2: static checks
make lint                            # golangci-lint
make gen && git diff --exit-code     # generated code matches api/openapi.yaml
cd web-v2 && npx tsc -b && npx eslint .

# Dashboard unit tests (Vitest)
cd web-v2 && npm test

# Layer 3: Playwright (needs a running API and dashboard; see below)
cd web-v2 && npm run test:e2e
```

Use `npx tsc -b` for type checks. `tsc --noEmit` checks nothing in this repo.

[CONTRIBUTING](../CONTRIBUTING.md#what-ci-runs-on-your-pr) lists every CI
gate and its local command.

## Playwright: how the end-to-end tests work

- **Where:** `web-v2/playwright.config.ts`, specs in `web-v2/e2e/*.spec.ts`,
  seed code in `web-v2/e2e/fixtures/`. Chromium only.
- **What runs:** the API (`go run ./cmd/velox`) and the built dashboard
  (`vite build`, then `vite preview`, which forwards `/v1` to the API).
- **Settings:** `E2E_BASE_URL` (default `http://127.0.0.1:4173`),
  `E2E_API_URL` (default `http://127.0.0.1:8080`), `E2E_API_KEY`,
  `E2E_EMAIL` and `E2E_PASSWORD`. The key and the sign-in come from
  `velox-bootstrap`.
- **Smoke spec:** `smoke.spec.ts` opens every dashboard page and the public
  hosted invoice page, at 1440px and 390px wide. Each page must show its
  heading, log no console errors, get no failed `/v1` response, and not
  scroll sideways. There is one test per page, so a failure names the page.
- **Journeys:** sign in, create a customer, subscribe it, advance a test
  clock to the next renewal and see the invoice, open the hosted invoice
  page, grant credits and see the balance.
- **In CI:** the `e2e` job runs on every PR. It is not a required check yet.
  On a failure, download the `playwright-report` artifact. It holds a trace
  you can open with `npx playwright show-trace`.

### Seed data

The tests create their own data through the public API, using the test-mode
secret key from `velox-bootstrap`. The seed code is TypeScript in
`web-v2/e2e/fixtures/` and stops at the first API error. There are no shell
scripts and no SQL inserts, so the seed follows the same rules a real
integrator does.

Each run starts from an empty database. Locally, create your own database
for this. Never point the tests at the `velox` database you use for
development.

### Flaky tests

A flaky test passes and fails on the same code.

- CI retries a failed test once. A test that passes on the retry is
  reported as flaky, not as passed.
- Before you commit a spec, run it five times: `npx playwright test
  --repeat-each 5 <spec>`.
- Wait on the page or the network, never on a fixed sleep.
- Select by role and label (`getByRole`, `getByLabel`), not by CSS class.
- Fix or delete a flaky spec. Do not mark it as skipped and move on.

## Checking a change by hand

Automated tests come first. When you also check something by hand, three
habits have caught real bugs here:

- **Look at the screen.** Text and DOM checks cannot see layout bugs. A UI
  change is not done until you have looked at a screenshot of it, in the
  state a user would see.
- **Check money at the provider.** For a payment or refund, confirm the
  amount in the Stripe dashboard, not only in Velox.
- **For a fix, see the test fail without it.** Revert the fix, run the new
  test, watch it fail, then restore the fix. A test that passes either way
  proves nothing.

## Before a release

Run the [release checklist](ops/release-checklist.md). It covers what only a
real provider can show: a Stripe charge, refund and webhook, an email you
open and read, the LiteLLM callback, and the invoice PDF.

The golden-path demo (`scripts/demo.sh`) and the `velox-doctor` sweep also
run in CI on every PR. The doctor checks the money invariants over the rows
the golden path left behind.

## Old flow IDs

Until October 2026, hand-run test steps lived in `MANUAL_TEST.md`, with flow
IDs such as `FLOW TC4` or `FLOW B10`. Some code comments and ADRs still name
them. Commit `a75d02d5` is the last commit that has the file:

```bash
git show a75d02d5:MANUAL_TEST.md | less
```
