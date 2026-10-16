Paused as asked. Both branches are committed and pushed, and I stopped my vite (5182) and API (8182) processes. I've opened one PR so far; the second branch is unfinished work.

| Branch | Last commit | Pushed | PR |
|---|---|---|---|
| `ux/subs-preview-proration` (base `feat/app-shell`) | `f21afb42` | yes | **#1096** https://github.com/getvelox/velox/pull/1096 |
| `ux/subs-dates-preview-card` (base `ux/subs-preview-proration`) | `183a784e` (WIP) | yes | none |

**#1096: the next-invoice preview now prorates like the invoice (M-SUB-1).**
- It's ready for review, body saved at `/tmp/claude-0/ux/build-subs/pr1-preview-proration.md`.
- The base-fee code that real invoices use was moved out of the cycle close unchanged, and the preview now calls the same code.
- Live API check: globex-seats previewed $120.00 and now previews "Team seats - base fee (qty 8, prorated 19/31 days)" at $73.55, which is what the invoice bills.
- The new real-Postgres test `TestPreview_MatchesCycleClose` compares the preview with the closed invoice line for line in 10 scenarios. I checked it catches regressions: going back to the old preview breaks 7 scenarios, and rating usage over the whole period breaks 1.
- Short suite, gofmt, vet, golangci-lint and the repo lints pass.
- In the billing integration run, two tests hit deadlocks and passed when re-run. `TestCloseVsThresholdFire_BothOrders_BillsUsageOnce` fails here and on `feat/app-shell` too: the shared local Postgres runs out of connections.

**`ux/subs-dates-preview-card` (WIP, typecheck and eslint clean):**
- A shared dates helper (`lib/subscriptionPeriod.ts`) for the summary, timeline, properties and cancel dialog. It covers M-SUB-3/4/5.
- The "Upcoming invoice" card shows its period, "estimate as of", server warnings, "$0.00 so far" when empty, and Retry on errors (M-SUB-2/8). The customer page's Upcoming card uses the same helper.
- The no-card notice is trial-aware (M-SUB-13).
- Copy changes (M-SUB-12): the button reads "Pause collection…" and the typed confirm offers "Keep subscription" / "Cancel subscription now". This needed a small additive `cancelLabel` prop on the shared `TypedConfirmDialog`.
- Helper tests pass.

**Next steps to resume:**
- **`ux/subs-preview-proration`:**
  1. Watch CI on #1096.
  2. If `feat/app-shell` moves, rebase and re-run the billing integration tests under the flock.
- **`ux/subs-dates-preview-card`:**
  1. Use the dates helper in the list's "Next bill" column, so canceled rows read "Ended …".
  2. Fix the mobile list header (M-SUB-11).
  3. Run `npm test`, `npm run build` and eslint.
  4. Take before/after screenshots (desktop light, dark, 390px) on vite 5182, pointed at a branch API on 8182, and look at them.
  5. Write the CHANGELOG entry, update MANUAL_TEST (`make lint-manual-test`), and rewrite the WIP commit message.
  6. Open the PR with base `ux/subs-preview-proration`.
- **Not started**, in this order:
  - Collection paused: the cause, a chip and a list filter, server and UI (M-SUB-6). It needs migration 0179; re-check every branch for that number before claiming it.
  - Test clock fixes: return `display_name` and refetch when an advance finishes (M-CLK-1/2).
  - Dialog fixes: plan pickers filtered by currency, meter name in the 409, inline field errors, quantity change logged as immediate (M-SUB-15/16/9).
  - Redesigns: subscription detail (C1), the subscriptions list with customer search, test clock detail and list (C2), and the create dialog (M-SUB-22).
- **Simplifying flows (your later instruction):** I haven't started this. I'll fold it into the redesign PRs and count steps against the flow table in `e2e.md`. The plan:
  - Cancel becomes one dialog. The typed CANCEL stays only for cancelling a paying subscription immediately; a scheduled cancel is one step and undoable.
  - The create dialog picks the plan first and fills in name and code.
  - Test clocks get an "advance to next renewal" option.
  - Each PR body will report "Flow: before N steps → after M steps".
- **Overlapping open PRs:** #1083 and #1085 touch the same pause dialog and test-clock presets, so I'll keep their logic when redesigning those screens.
