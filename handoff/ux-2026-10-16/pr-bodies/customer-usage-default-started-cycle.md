## What & why

**A new trial no longer hides the running cycle's usage.**

`usage.resolvePeriod` picks the default window for `GET /v1/customers/{id}/usage` and for the public cost dashboard, which calls it with an empty period. It took the active or trialing subscription with the latest `current_billing_period_start`. A trialing subscription stores its **first paid** period, which starts at trial end (`subscription/service.go`, `firstPeriodAfterTrial`).

Scenario:

- Sub A is active, cycle 2026-10-01 → 2026-11-01.
- On 2026-10-10, sub B starts a 14-day trial. Its stored period is 2026-10-24 → 2026-11-24.

| Default request on Oct 10–23 | period | A's October usage |
|---|---|---|
| Before | 2026-10-24 → 2026-11-24 (a future window) | $0 |
| After | 2026-10-01 → 2026-11-01 | priced |

From Oct 24 on, B's cycle has started and becomes the default, as before.

### Fix

`resolvePeriod` takes `now` and skips cycles whose start is after it. `now` is the customer's effective now, from `usage.Service.effectiveNow`: the same helper the usage summary uses, reached through `CustomerUsageService.usage`. It is the test-clock frozen time for a clock-pinned customer. The cost dashboard pins livemode on ctx before `Get`, so the lookup works there too. `now` is read only on the default path, so explicit windows cost no lookup.

**Sub-case: no cycle has started** (for example, the customer's only subscription is trialing). It keeps the previous pick: the latest start across all subscriptions, which is the future first paid period. Changing what a trial-only customer sees is a product decision (the trial window, the first paid period, or an error), so this PR leaves it as it was. `TestResolvePeriod` locks it.

Not changed (product decision): how several active subscriptions with different anchors are rated.

**Sibling with the same heuristic, not changed:** `billing.pickPrimarySubscription` (`internal/billing/preview_create.go`). Its comment says it "mirrors customer-usage's primary active subscription heuristic". It is used by create-preview when no `subscription_id` is given, and still picks a trial's future cycle. Left for a separate item because it is outside this item's scope.

OpenAPI: the `/v1/customers/{id}/usage` description now says "started last, as of the customer's current time". It also says a trialing subscription's cycle starts at trial end and is not picked before then, unless no cycle has started. `make gen` re-run.

### Tests

- `TestResolvePeriod` gains three cases:
  - future trial skipped on Oct 10;
  - trial picked on Oct 24;
  - trial-only customer falls back to the trial cycle.
- `TestCustomerUsageService_Get_DefaultSkipsNotYetStartedTrialCycle` runs through `Get` with a `fakeResolver` clock frozen at 2027-03-10. The dates are in the wall-clock future, so this also proves the clock wiring.

Mutation checks:

1. Skip disabled (`if false && …After(n)`):
   ```
   TestResolvePeriod/future_trial_cycle_is_skipped_while_another_cycle_runs: from: got 2026-10-24 …, want 2026-10-01 …
   customer_usage_test.go:407: default period: got 2027-03-24T00:00:00Z → 2027-04-24T00:00:00Z, want sub A's running cycle 2027-03-01T00:00:00Z → 2027-04-01T00:00:00Z …
   ```
2. Wiring reverted (`now` = `time.Now().UTC()`): the `Get` test fails with the same line 407 message.

Both pass with the fix restored.

### Checks

- `gofmt -l` clean; `go vet` clean; `golangci-lint` v2.12.2 (CI's pin) on `./internal/usage/...`: 0 issues.
- `make lint-clock lint-tz lint-manual-test lint-funding-set`: all ok.
- `go test ./... -short -count=1`: pass.
- `go test -p 1 -count=1 -short=false ./internal/usage/...`: pass.
- `make gen`: expected comment drift only, committed.
- web-v2 (generated files only): `npx tsc -b` ok; `npm test` 233 pass, 0 fail.

## Checklist

- [x] Tests cover the new behavior (unit; integration too if a store or money path changed)
- [x] `go test ./... -short -count=1` passes (run without `-race`)
- [x] `make lint` passes (golangci-lint v2.12.2 on the touched package)
- [x] `CHANGELOG.md` entry added — required for any user-visible change
- [x] `MANUAL_TEST.md` flow added/updated: FLOW CU2 box, auto-locked
- [x] `make gen` re-run — required if `api/openapi.yaml` changed
- [x] Frontend touched? Generated files only: `tsc -b` and `npm test` pass

## Money path only

This PR is a read path: it changes no state and fires no effects. The site-set below covers the one decision it changes, which default window gets rated.

- **Readers of the decision:** `CustomerUsageService.Get`, reached from:
  - `/v1/customers/{id}/usage`;
  - `CostDashboardAssembler.GetByToken` (public dashboard, empty period);
  - `MarginAssembler.Get` (always explicit bounds, so unaffected).
- **Sibling heuristic:** `billing.pickPrimarySubscription` (create-preview), not changed (see above).
- **Writers of `current_billing_period_start` for a trial:** `subscription.Service.Create` via `firstPeriodAfterTrial`. Not touched.
- **Invoicing:** does not use `resolvePeriod`. Not affected.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
