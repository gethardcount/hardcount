## What & why

Resolves **M-SUB-1** (SUB-12, E2E-1): the subscription page's next-invoice preview showed the full base fee where the cycle close bills a prorated one.

- **Before:** globex-seats (calendar billing, created Oct 13) previewed `Team seats - base fee (qty 8)` **$120.00**. The close bills `prorated 19/31 days` = **$73.55**. A mid-period item add or quantity change also previewed the full fee, plus a warning that the estimate "excludes mid-period proration".
- **After:** the default-window preview mirrors the cycle close. It prorates the same way and shows the same line text as the invoice ("prorated 19/31 days").

How:
- **Shared base-fee math.** The close's base-fee block moved out of `buildLineItems` unchanged into `closeBaseFeeLines` (`internal/billing/close_base_lines.go`). Both the close and the new `previewCycle` call it.
- **Same usage windows.** Usage is rated over the windows the close bills (`meterUsageIntervals`). A meter added mid-period no longer counts usage from before the add.
- **Same close inputs.** The preview also reads the threshold watermark, the invoice currency and the scheduled-cancel boundary, as the close does.
- **Unchanged paths.** The threshold scan and `create_preview` with an explicit `period` keep the flat estimate (`previewWithWindow`). The threshold fire depends on the unprorated base line. A trialing subscription also keeps the flat preview.
- **Gaps that stay named.** The usage cap is still not replicated (existing warning). After a spend-threshold fire, the preview shows what is left and says tiered and max/last meters may differ.

API: additive only. The `create_preview` description in `api/openapi.yaml` is updated. `make gen` is re-run; it changes descriptions only.

| | Before | After |
|---|---|---|
| globex-seats (calendar stub Oct 13 → Nov 1) | `Team seats - base fee (qty 8)` $120.00 | `… (qty 8, prorated 19/31 days)` $73.55 |
| ux-sub-main (calendar stub, qty changed mid-period) | $117.00 base + "excludes mid-period proration" warning | `… (qty 3, prorated 16/31 days)` $60.39, no warning |

## Evidence

The before screenshots come from the main stack (5173 → API 8080). The after screenshots come from this branch (vite 5182 → this branch's API on 8182, same dev DB). They are in `/tmp/claude-0/ux/build-subs/pr1-preview-proration/`, for desktop light, desktop dark and 390px.

- **globex-seats, before:** the preview card reads "Team seats - base fee (qty 8)", 8, $120.00, Subtotal $120.00.
- **globex-seats, after:** the card reads "Team seats - base fee (qty 8, prorated 19/31 days)", $73.55.
- **ux-sub-main, after (390px):** the line reads "ux-plan-edited - base fee (qty 3, prorated 16/31 days)". At 390px the card's amount column is cut off; that is an existing layout problem, fixed in the preview-card PR that follows.

Live API check (`GET /v1/billing/preview/{id}`):
- **main:** `[('Team seats - base fee (qty 8)', 12000)]`.
- **this branch:** `[('Team seats - base fee (qty 8, prorated 19/31 days)', 7355)]`.

## Tests

New: **`TestPreview_MatchesCycleClose`** (real Postgres). Each scenario previews mid-period, then closes the period with `RunCycleForTenant`. It then compares preview and invoice line for line (type, description, quantity, amount) and pins the total.

The scenarios are every cycle-close shape of the ADR-101 corpus: full period, creation-day clamp, mid-period add, quantity change, immediate plan swap, scheduled swap, removal, same-instant add+swap. Two more are added: a **calendar stub first period** (12000 × 19/31 = 7355) and a **metered item added on day 10**, with usage before and after the add.

Mutation checks:
1. **Revert to the flat preview** (`previewCycle` → `previewWindow(..., false)`). 7 of 10 scenarios fail at `preview_cycle_parity_integration_test.go:189` ("preview diverges from the invoice the close issued"): mid-period-add, quantity-change, plan-swap-immediate, remove-mid-period, same-instant-add-and-swap, calendar-stub-first-period and mid-period-add-with-usage.
2. **Rate usage over the whole period** instead of the close's windows. `mid-period-add-with-usage` fails: preview `usage|Calls (calls)|12|120` vs invoice `…|7|70`.

Updated:
- **Preview fixture.** It now writes the item's `billing_intervals` row, as every real writer does.
- **`TestCreatePreview_EstimateScopeWarnings` mid-period case.** It now uses an explicit `period`, where the flat estimate still warns.

## Checklist

- [x] Tests cover the new behavior (integration, real Postgres)
- [x] `go test ./... -short -count=1` passes
- [x] `gofmt`, `go vet ./internal/billing/`, `golangci-lint run ./internal/billing/...`: 0 issues
- [x] `make lint-clock lint-tz lint-funding-set lint-manual-test`: all ok
- [x] Billing integration tests, run with `flock /tmp/claude-0/pg.lock go test -p 1 -count=1 -short=false ./internal/billing/`: all pass except three. The two deadlock failures (`TestBillingFailure_ParksAtTheEighthFailure`, `TestImmediateCancel_TrialNeverActivated_PastPeriodEnd_StillFree`) passed on a rerun. `TestCloseVsThresholdFire_BothOrders_BillsUsageOnce` fails the same way on the base branch: the local Postgres runs out of connection slots (53300) while several sessions and API instances share it.
- [x] `CHANGELOG.md` entry (Fixed)
- [x] `MANUAL_TEST.md`: the S2.4 preview boxes are rewritten. "The default preview prorates like the invoice" is `[x]` (`TestPreview_MatchesCycleClose`). "An explicit period is a flat estimate and says so" is `[x]` (`TestCreatePreview_EstimateScopeWarnings`).
- [x] `make gen` re-run (description-only change)

## Money path only

The state is the base-fee and usage lines of a subscription period. Site-set (from `grep -n "emitBaseSegmentLine\|previewWithWindow\|\.Preview(\|CreatePreview(" internal/`):

- **Writers of base-fee lines:**
  - `buildLineItems` (cycle close) now calls `closeBaseFeeLines`. The block was moved unchanged and the existing corpus `TestIntervalReader_Corpus` passes.
  - `buildCancelLineItems` (cancel) and `buildOnCreateInvoice` are untouched.
  - The threshold fire uses `previewWithWindow` (flat). It is untouched and still prorates reset-mode bases itself.
- **Readers of the preview:**
  - `billing.Handler.preview` (`GET /v1/billing/preview/{id}`) → `Engine.Preview` → `previewCycle` (changed).
  - `PreviewService.CreatePreview` → `previewCycle` when no `period` is given, otherwise `previewWithWindow` (unchanged).
  - `evaluateThresholds` → `previewWithWindow` (unchanged).
- **Effect-firers:** none. The preview writes nothing (`TestCreatePreview_NoWrites` still passes).
- **Crash points:** none. Read-only.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
