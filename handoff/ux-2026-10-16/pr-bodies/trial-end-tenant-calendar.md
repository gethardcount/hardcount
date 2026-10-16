## What & why

**`trial_days` now counts calendar days in the billing timezone.** `Create` computed `trial_end_at` as `now.AddDate(0, 0, trial_days)` on a UTC instant, which adds N×24 hours. Across a DST change that lands one hour off in the tenant zone. Near midnight it crosses to the next civil day, and `firstPeriodAfterTrial` then snaps the first paid period to the wrong day.

Scenario (America/New_York tenant, `trial_days=14`, created 2026-03-01 23:30 EST = `2026-03-02T04:30Z`; DST starts Mar 8):

| | `trial_end_at` | In New York | First paid period starts |
|---|---|---|---|
| Before | `2026-03-16T04:30:00Z` | Mar 16 00:30 EDT | Mar 16 |
| After | `2026-03-16T03:30:00Z` | Mar 15 23:30 EDT | Mar 15 |

Change: `te := domain.AddDaysIn(now, input.TrialDays, loc)`, where `loc` is the tenant location `Create` already resolves for the period snap.

What changes beyond the hour: only the case above, where the hour moves `trial_end_at` across civil midnight. There the first paid period starts one day earlier (the correct day), so a calendar-billed stub covers one more day. Outside a DST change the result is identical. UTC tenants are unchanged.

**Shared helper.** `domain.AddDaysIn` and `internal/domain/add_days_in_test.go` are byte-identical to the copies on `fix/due-date-tenant-calendar` (also from origin/main). `git merge-tree` of the two branches conflicts only in `CHANGELOG.md`. Whichever PR merges second rebases and keeps both CHANGELOG entries.

Not done: no backfill of existing `trial_end_at` values.

### Tests

- `internal/subscription/trial_end_tenant_calendar_test.go` `TestCreate_TrialDays_UsesTenantCalendarAcrossDST`: the scenario above, asserts `trial_end_at` and the first period start.
- `internal/domain/add_days_in_test.go` `TestAddDaysIn_CrossesDSTOnCivilCalendar` (shared with the due-date branch).

Mutation check (reverted the one line to `now.AddDate(0, 0, input.TrialDays)`, ran, restored):

```
trial_end_tenant_calendar_test.go:37: trial_end_at = 2026-03-16 04:30:00 +0000 UTC, want 2026-03-16T03:30:00Z (Mar 15 23:30 EDT)
trial_end_tenant_calendar_test.go:40: first paid period start = 2026-03-16 04:00:00 +0000 UTC, want 2026-03-15T04:00:00Z (Mar 15 00:00 EDT)
```

### Checks run

- `gofmt -l internal/ cmd/`: empty.
- `go vet ./internal/domain/... ./internal/subscription/...`: ok.
- `golangci-lint run` (v2.12.2, the CI pin, built with go1.26) on the same packages: `0 issues.`
- `make lint-clock lint-tz lint-manual-test lint-funding-set`: all ok.
- `go test ./... -short -count=1`: all ok.
- `go test -p 1 -count=1 -short=false` on domain, subscription, billing (real Postgres): all ok.

## Checklist

- [x] Tests cover the new behavior (unit; integration too if a store or money path changed)
- [ ] `go test ./... -race -short -count=1` passes (ran without `-race`; passes)
- [x] `make lint` passes (golangci-lint v2.12.2 on touched packages: 0 issues)
- [x] `CHANGELOG.md` entry added — required for any user-visible change
- [x] `MANUAL_TEST.md` flow added/updated — FLOW TC6, new `[x]` box under "Calendar + monthly + trial"

## Money path only

State: `subscriptions.trial_end_at` and the first post-trial period it anchors. Site-set from
`grep -rn "TrialEndAt\|trial_end_at\|TrialDays\|AddDate(0, 0" internal --include=*.go`:

**Writers of `trial_end_at`:**

- [x] `internal/subscription/service.go` `Create` (~796): derives it from `trial_days`. **Fixed.**
- [x] `Service.ExtendTrial` → `PostgresStore.ExtendTrial` (`postgres.go` ~1006): takes an explicit RFC3339 `trial_end` from the API. No day math. Unchanged, as briefed.
- [x] `Service.EndTrial` → `endTrialEarlyInTx` (`postgres.go` ~947): stamps the current instant. No day math. Unchanged.
- [x] Recipes (`internal/recipe`), web-v2 and other packages: no `trial_days` → date computation (grep empty). The dashboard sends `trial_days` to the API.

**Gated readers (read the stored instant, unchanged):** engine trial flip `billing/engine.go` ~2535 and `handleTrialState`, `billing/live_balance.go` ~136/156, `domain/subscription.go` ~538 (cancel-at-trial-end), test-clock catch-up `testclock/postgres.go` ~401, CAS predicate `subscription/postgres.go` ~2990 (`AND trial_end_at = $3`, compares to the stored value, so unaffected).

**Effect-firers / crash points:** none added. `subscription.trial_ended` and the post-trial bill fire at the stored `trial_end_at`, now on the correct civil day. The change is a value computed before the existing create transaction.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
