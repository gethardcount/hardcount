## What & why

**Customer usage accepts a full leap year.**

`resolvePeriod` capped explicit windows with `period.To.Sub(period.From) > 365*24h`. The error text and `api/openapi.yaml` both say "one year", but a leap year is 366 days.

| Request | Before | After |
|---|---|---|
| `GET /v1/customers/{id}/usage?from=2028-01-01&to=2028-12-31` | 422 `window must be ≤ 1 year` | 200, `period.source: "explicit"` |
| explicit yearly cycle `2028-01-01T00:00:00Z` → `2029-01-01T00:00:00Z` (366 days; also via `/v1/customers/{id}/margin`, which passes explicit bounds) | 422 | 200 |
| `2028-01-01T00:00:00Z` → `2029-01-01T00:00:01Z` | 422 | 422 (unchanged) |

Fix: `if period.To.After(period.From.AddDate(1, 0, 0))` → 422. `MaxCustomerUsageWindow` had no other reference (grep of the repo), so it is removed; the reason it existed (bounded LATERAL JOIN cost, `docs/design-customer-usage.md`) moved into the comment at the check. Two doc comments in the same file said 400; the handler answers 422 (`errs.Invalid` → `respond.FromError`), so they now say 422. The comment also no longer says the window must be in the past, which was never checked.

No OpenAPI change: "longer than one year" is now true as written.

### Tests

`TestResolvePeriod` gains three cases: leap year 2028 (date-only end at 23:59:59.999999), a 366-day explicit yearly cycle, and one calendar year plus 1µs (still rejected).

Mutation check (old `365*24h` check restored):

```
--- FAIL: TestResolvePeriod/leap_calendar_year_2028_is_within_the_one-year_cap
    customer_usage_test.go:252: unexpected error: window must be ≤ 1 year
--- FAIL: TestResolvePeriod/366-day_yearly_cycle_passed_explicitly_is_accepted
```

Pass with the fix restored.

### Checks

- `gofmt -l` clean; `go vet ./internal/usage/...` clean; `golangci-lint` v2.12.2 (CI's pin): 0 issues.
- `make lint-clock lint-tz lint-manual-test lint-funding-set`: all ok.
- `go test ./... -short -count=1`: pass.
- `go test -p 1 -count=1 -short=false ./internal/usage/...`: pass.

## Checklist

- [x] Tests cover the new behavior (unit; integration too if a store or money path changed)
- [x] `go test ./... -short -count=1` passes (run without `-race`)
- [x] `make lint` passes (golangci-lint v2.12.2 on the touched package)
- [x] `CHANGELOG.md` entry added — required for any user-visible change
- [x] `MANUAL_TEST.md` flow added/updated: new FLOW CU2 box, auto-locked by `TestResolvePeriod`

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
