# Code standard

How Hardcount code differs from standard practice. For the rest we follow [Effective Go](https://go.dev/doc/effective_go), [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments), and the [React](https://react.dev/learn) and [TanStack Query](https://tanstack.com/query/latest/docs/framework/react/overview) docs.

New and changed code follows this page. Older code does not all follow it yet; small cleanup PRs bring it in line. A change to money or a billing state also follows the [money-path playbook](money-path-robustness-playbook.md).

## Go rules a tool checks

[CI runs these](../../CONTRIBUTING.md#what-ci-runs-on-your-pr). forbidigo and nolintlint are linters in [`.golangci.yml`](../../.golangci.yml). The rest are tests in [`internal/arch`](../../internal/arch), and each top comment says how to pass.

- Where records can run on a [test clock](../glossary.md#testing-with-simulated-time), time comes from the clock, not `time.Now()`: `s.clock.Now(ctx)` in a service, `clock.Now(ctx)` in a store. `.golangci.yml` lists the packages. (forbidigo)
- In `internal/subscription` and `internal/billing`, a credit or void for a whole billing period uses `FindFundingInvoicesForPeriod`, because one period can have several invoices. A real single-invoice case says why: `//nolint:forbidigo // single-invoice-ok: <reason>`. (forbidigo)
- A lint waiver names its linter and a reason: `//nolint:forbidigo // wall-clock: <reason>`. (nolintlint)
- A calendar date shown to a person is formatted in a named time zone: `t.In(loc).Format(...)`. A real UTC or filename format carries a same-line `//tz:ok`. (`civil_date_timezone_test.go`)
- `internal/api/respond` writes every error response and decodes every request body Hardcount defines. Return an `errs.DomainError`, and `respond.FromError` picks the status and error code. (`error_writes_test.go`, `respond_error_args_test.go`, `request_body_decode_test.go`)
- The SQL behind a list route has no fixed `LIMIT`: it takes `limit` and `offset` from `respond.ParsePage`. (`sql_row_caps_test.go`)
- Every setting the code reads has a row in [settings.md](../settings.md), and the ones `config.Load` owns are read only in `internal/config`. (`settings_doc_test.go`, `config_env_reads_test.go`)

## Go rules checked in review

### Money, tenants and the API

- **Money is `int64` cents, in a field named `..._cents`.** Never a float, even to print it. Round with `internal/platform/money`, which rounds half to even.
- **Tenant data goes through a tenant transaction:** `db.BeginTx(ctx, postgres.TxTenant, tenantID)`. The exceptions are in [Architecture](../architecture.md#design-rules).
- API JSON fields are `snake_case`, and an ID carries its type: `postgres.NewID("hc_cus")` gives `hc_cus_...`.
- A store returns `errs.ErrNotFound` for a missing row. `sql.ErrNoRows` never leaves the store.

### Wiring and config

- A constructor takes every dependency: a nil field that means "skip this step" skips work in silence. Use a `Set...` method only to break a real construction cycle.
- Only `internal/config` and `cmd/*/main.go` read the environment. Each setting is parsed there once, with its default, and code that receives it adds no fallback.
- A compose file does not repeat a default: write `${VAR:-}`, so the default in Go applies.
- No code exists only for a test: no branch, flag, constructor or package variable that a test flips. Tests use the interfaces production uses.

### Errors, state and structure

- If you drop an error, write `_ =` and the reason on the same line. A deferred `rows.Close()` or `tx.Rollback()` needs no comment.
- **Guard a state change in the write itself.** Name the states the row may come from (`WHERE status = ANY($1)`), not the ones it may not (`status <> 'voided'`): a deny-list lets in a state added later. Then check the row count: zero means another writer won. A check read before the write can be stale. See also [playbook item 5](money-path-robustness-playbook.md#d-race).
- Keep a function under about 200 lines and a file under about 3,000. Split money code only in a PR that changes nothing else.
- Pass a service straight in where it fits the caller's interface. Write an adapter, in `internal/api/adapters.go`, only when the types differ.
- Delete what no production path reaches, even if a test calls it: code, scripts, configs.
- A new dependency needs a reason in the PR.
- Log with `slog.InfoContext` and its siblings when you have a context: the log handler adds `request_id` and `tenant_id`.

### Work that outlives a request

Work that must finish after its request ends runs on `context.WithoutCancel(ctx)` with a timeout. `context.Background()` drops the request ID and the tenant.

A cancel during COMMIT can apply the commit and still return an error. So a transaction whose follow-up steps must run takes its own `commitBudget` context (30s) just before it starts, and nothing else runs on it:

```go
writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitBudget)
defer cancel()
err := s.store.Save(writeCtx, tenantID, inv) // one transaction
```

Create one `stepsBudget` context (5 minutes) after the commit, for every step that follows. A function that calls Stripe between two of its transactions, such as a refund, creates that context before its first transaction. Each budget is unexported in its package: copy the constants `stepsBudget` in `internal/invoice/service.go` and `commitBudget` in `internal/creditnote/service.go`.

Every goroutine has an owner that waits for it at shutdown, or a comment naming the job that retries its work.

## Frontend (web-v2)

- New API calls use the client and types from orval, the generator `make gen` runs on `api/openapi.yaml` ([workflow](openapi-workflow.md)). A PR that changes how a page fetches data moves the whole page to that client. Add nothing to `src/lib/api.ts`.
- **Never show a guessed value.** No `?? 'USD'` or `|| 'UTC'` on a required field, and no `catch` that returns an empty list. Show the loading or error state.
- Decisions live in pure functions in `src/lib`, each with a Vitest test: labels, dates, money, who may do what.
- Type a status with its generated union and handle every value, so a new status fails `tsc -b`.
- A money dialog keeps its double-click guard and its [idempotency key](../glossary.md#pricing-and-usage).
- A hand-written request uses `fetchWithTimeout` from `src/lib/api.ts`, never a raw `fetch`.
- Do not disable the react-hooks eslint rule or hide an unused export from knip (the unused-code checker).

## Scripts and CI

- A new CI check fails the build, unless its PR says it only annotates, as `doc-style` does.
- A workflow follows GitHub's [secure use guide](https://docs.github.com/en/actions/reference/security/secure-use). Every job has `timeout-minutes`, and every image is pinned to a version tag.
- An image is scanned and signed before any public tag points at it.
