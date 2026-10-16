## What & why

Stacked on #1092 (`ux/customers-currency`). Resolves **M-CUST-14**.

`MarginAssembler.Get` computed `(revenue − cost) / revenue` whenever revenue > 0. When no event in the window carries a stamped provider cost (no rates set up, or none matched), cost is 0 and the formula reads **100.0%**. Globex showed "Usage billed $195 · Provider cost $0.0000 · Margin 100.0%".

- `internal/usage/provider_cost.go`: `MarginReport` gains `costed_events` (events with a stamped cost, zero cost included) and `cost_rates_configured` (tenant has any provider rate; new `HasProviderCostRates`, RLS-scoped). `margin_bps` is omitted when `costed_events == 0`.
- OpenAPI updated (`make gen`, generated files committed, no drift). **API:** additive fields; `margin_bps` is now also omitted in that case (it was a fabricated 100%). Called out in the CHANGELOG.
- `web-v2/src/lib/marginCost.ts` → one state (`measured`/`no-rates`/`no-match`/`no-usage`); MarginCard shows "—" for cost and margin plus "No provider costs configured, so there is no margin to show. Set up provider costs" or "No provider rate matched this usage, so its cost and margin are unknown. Provider costs".

| | Before | After |
|---|---|---|
| Globex (usage, rates exist, none match) | Provider cost $0.0000 · Margin 100.0% | Provider cost — · Margin — · "No provider rate matched this usage…" + link |
| Tenant with no rates | 100.0% | — · "No provider costs configured…" + "Set up provider costs" |
| ACME (15 costed events, control) | 67.07% | 67.07% (`margin_bps 6707`, `costed_events 15`, checked on the branch API) |

## Evidence

`/tmp/claude-0/ux/build-customers/margin/` — before from main (:5173), after from this branch's vite (:5181) proxied to this branch's API build on :8181:
- `before-globex__light.png`: $195.00 / $0.0000 / 100.0%.
- `after-globex__light.png`, `__dark.png`, `__mobile.png`: $45.00 (current period, ADR-121) / — / —, sentence and Provider costs link; readable in dark; at 390px the three figures fit and the sentence wraps.

## Tests

- `TestMargin_NoProviderCostMeansNoMargin` (real Postgres): revenue 200, no costed event → no margin, `costed_events 0`, `cost_rates_configured false`; after seeding a non-matching rate → configured true, still no margin. Mutation (drop `&& rep.CostedEvents > 0`) → `margin_integration_test.go:245: margin_bps = 10000, want none: no event carries a provider cost`.
- Existing `TestMargin_AttributionHonesty` / `TestMargin_CurrencyIsNamedAndNeverMixed` still pass (costed windows keep their margin).
- `web-v2/tests/marginCost.test.ts` (4). Mutation (`> 0` → `>= 0`) → `not ok 1 - usage but no rates at all → no-rates, with the setup sentence` (+2 more).

## Checklist

- [x] gofmt clean, `go vet ./internal/usage/`, golangci-lint `./internal/usage/...` 0 issues
- [x] `make lint-clock lint-tz lint-funding-set lint-manual-test` OK
- [x] `go test ./... -short -count=1` all ok; `flock … go test -p 1 -short=false ./internal/usage/...` ok
- [x] `make gen` run; re-run shows no drift
- [x] CHANGELOG (Fixed, with API note); MANUAL_TEST FLOW CU10 box `[x]` with tests named
- [x] Web: tsc, eslint (0), `npm test` 260 pass, build OK

## Money path only

Read-only report; no state machine, writer or effect-firer touched. Gated readers of `margin_bps`: `MarginCard` (only consumer in web-v2; grep `margin_bps`). No other server consumer of `MarginAssembler.Get` (handler `Margin` only).

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
