## What & why

**A margin request without dates follows the customer's test clock.**

`usage.parseWindow` defaulted a missing bound to `time.Now().UTC()` minus 30 days. The usage summary (`usage/summary.go`, `h.svc.effectiveNow`) and customer usage follow the customer's simulated now.

Scenario: test-mode customer on a test clock advanced to 2027-03-01, today 2026-10-16, bare `GET /v1/customers/{id}/margin`.

| | window | revenue / cost |
|---|---|---|
| Before | 2026-09-16 → 2026-10-16 (wall clock) | 0 / 0, every simulated event is outside the window |
| After | 2027-01-30 → 2027-03-01 (clock time) | the simulated usage in that window |

Fix: `ProviderCostHandler.marginWindow` passes `parseWindow` a `now` callback that resolves `MarginAssembler.effectiveNow` → `CustomerUsageService.usage.effectiveNow`. That is the same `usage.Service.effectiveNow` the summary handler calls, reused, not copied. No router change: the router already builds the margin assembler on the `customerUsageSvc` that holds `usageSvc`, which gets `SetResolver(engine)`. Unchanged:

- the 30-day length;
- live mode, which `simNow` keeps on the wall clock with no lookup;
- explicit windows, where `now` is never called, so they cost no clock lookup.

`api/openapi.yaml` (`/v1/customers/{id}/margin`) now documents:

- the window `[from, to)`;
- the default (last 30 days ending at the customer's current time, the test clock time for a clock-pinned customer);
- RFC 3339 only, and `from` must be before `to`;
- `400` (parse error or `from` not before `to`, `respond.BadRequest`), `404` (unknown customer, from the usage read) and `422` (window over one year, from `resolvePeriod`).

`make gen` re-run. The generated react-query `TError` for this hook went from `unknown` to `Error` because error responses are now declared; `tsc -b` passes.

Not changed, needs a decision: the default `to` equals the customer's now and is exclusive. An event sent without a timestamp to a clock-pinned customer is stamped at exactly `frozen_time`, so the bare API default leaves out that newest event. The dashboard works around this with `to = now + 1ms` (`web-v2/src/lib/effectiveNow.ts rollingWindow`). The brief said to keep the default unchanged, so the API keeps `to = now`.

Dashboard not changed (it already sends explicit clock-following bounds).

### Tests

`TestMarginDefaultWindow_FollowsCustomerTestClock` (unit, `fakeResolver` stub clock), through `ProviderCostHandler.marginWindow`:

- pinned bare request → `[frozen-30d, frozen]`;
- unpinned → wall-clock, 30 days long;
- explicit window → bounds untouched, 0 resolver calls;
- only `from` → `to` = frozen.

Mutation check (callback reverted to `time.Now().UTC()`):

```
margin_window_test.go:37: to: got 2026-10-16T08:37:41Z, want the clock's frozen_time 2027-03-01T00:00:00Z
margin_window_test.go:40: from: got 2026-09-16T08:37:41Z, want 2027-01-30T00:00:00Z (30 days before frozen_time)
```

Passes with the fix restored.

### Checks

- `gofmt -l` clean; `go vet` clean; `golangci-lint` v2.12.2 (CI's pin) on `./internal/usage/...`: 0 issues.
- `make lint-clock lint-tz lint-manual-test lint-funding-set`: all ok.
- `go test ./... -short -count=1`: pass.
- `go test -p 1 -count=1 -short=false ./internal/usage/...`: pass.
- `make gen`: expected drift only (spec copy, Go + TS generated), committed.
- web-v2 (generated files only): `npx tsc -b` ok; `npm test` 233 pass, 0 fail.

## Checklist

- [x] Tests cover the new behavior (unit; integration too if a store or money path changed)
- [x] `go test ./... -short -count=1` passes (run without `-race`)
- [x] `make lint` passes (golangci-lint v2.12.2 on the touched package)
- [x] `CHANGELOG.md` entry added — required for any user-visible change
- [x] `MANUAL_TEST.md` flow added/updated: FLOW M1 box, `[ ]` (the unit test locks the window; the response echo needs a walk)
- [x] `make gen` re-run — required if `api/openapi.yaml` changed
- [x] Frontend touched? Generated files only: `tsc -b` and `npm test` pass

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
