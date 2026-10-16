I've paused. The working tree was already clean, so I made no WIP commit; every branch is committed and pushed. I opened three PRs, all stacked on `feat/app-shell`. I also stopped my own API build on :8181 and my vite on :5181. Nothing on :5173 or :8080 was touched.

| Branch | Last commit | Pushed | PR |
|---|---|---|---|
| `ux/customers-currency` (base `feat/app-shell`) | `0cbcc941` | yes | #1092 https://github.com/getvelox/velox/pull/1092 |
| `ux/customers-margin-no-costs` (base #1092's branch) | `02d244b2` | yes | #1095 https://github.com/getvelox/velox/pull/1095 |
| `ux/customers-credits` (base #1095's branch) | `c524bf6b` | yes | #1098 https://github.com/getvelox/velox/pull/1098 |

All three branches live in one worktree: `/home/user/velox/.claude/worktrees/ux-customers`. PR bodies are saved as `currency.md`, `margin.md` and `credits.md` in `/tmp/claude-0/ux/build-customers/`. Screenshots are in the subfolders `currency/`, `margin/` and `credits/` there.

**What each PR does:**
- **#1092** fixes M-CUST-15 and the customer-page part of M-X-11. It adds one customer-currency resolver: live plan, then billing profile, then latest invoice, then workspace default. A EUR-plan customer now reads € instead of $ for credit balance, usage total, margin and the New invoice default. It also contains the merge of #1089.
- **#1095** fixes M-CUST-14. The server now leaves out the margin when no usage event carries a provider cost, instead of reporting 100%. The Margin card shows "—" and says whether no rates are set up or none matched. There are two new additive API fields, `costed_events` and `cost_rates_configured`. `margin_bps` is now also left out in that case, which the CHANGELOG notes as an API change.
- **#1098** fixes M-CUST-16. The customer page shows granted, used and expired credit totals, used-up grants behind a toggle, the latest ledger entries and a Grant credits button. Invoice rows explain credit applied, which accounts for the gap between Outstanding and the invoice total. Grant credits goes from 6 steps to 4.

**Checks:** tests, mutation checks, CHANGELOG and MANUAL_TEST updates were done for all three. #1095 also passed the Go unit tests, the `internal/usage` integration tests, golangci-lint, the repo lint targets and `make gen` with no drift.

**Two small cross-area edits you should know about:**
- `pages/Credits.tsx` now exports its grant dialog, with an optional currency prop.
- `SubscriptionDetail.tsx` now refreshes the customer's subscription list under its new name; one line changed.

**Next steps to resume:**
1. **#1092, #1095, #1098:** nothing pending; they wait for review. On a later rebase, keep both sides' CHANGELOG and MANUAL_TEST entries. All three add boxes to the new MANUAL_TEST flow CU10 ("Customer page money figures").
2. **Restart the tools.** Create each new branch from `ux/customers-credits` in the same worktree. Rebuild the branch API and run it on :8181 with `DATABASE_URL=… APP_ENV=local PORT=8181`. Start vite with `VELOX_API_TARGET=http://localhost:8181 npx vite --port 5181 --strictPort`. The screenshot helper is `/tmp/claude-0/ux/build-customers/shoot.cjs`, and `cl.py` in the same folder inserts CHANGELOG entries.
3. **Correctness PRs not started:**
   - **M-CUST-1/2/3/4, emails.** Show `last_error` for any status, show "Not sent — SMTP not configured" with a Settings link, add the retry number, link to `/invoices/:id`, and fix the test-clock banner wording. If outbox retry behaviour changes, list every code site that touches that state first, as the money-path playbook requires.
   - **M-CUST-17, cost-dashboard URL.** Needs a token-status read on the server. Then Generate vs Rotate… with a confirm, label it an API endpoint, and drop the `0001-01-01` period.
   - **M-CUST-21/22, New invoice.** Enter money in dollars with `amountInputToCents`, and seed payment terms from the workspace's `net_payment_terms`.
   - **M-CUST-23, add payment method without Stripe.** Return a typed 409 from `internal/api/adapters.go` and show "Connect Stripe first" in the dialog.
   - **M-CUST-7/9/13.** Compact chart y-axis, "Usage charges (est.)" wording with a trial note, and Upcoming "Last invoice" excluding drafts with the shared status vocabulary.
4. **Redesigns not started:**
   - Customer detail per wireframe C1: header, KPI strip, notice stack, tabs and right rail. Keep the end-date behaviour from #1069 if I rewrite the Activity header. The credits section from #1098 becomes the Credits tab.
   - Customers list per wireframe C3: needs billing columns from the server, plus status filters and mobile cards.
   - Fold in ISSUE items M-CUST-5, 24, 25, 26, 27 and 28 where that code is rewritten.
5. **Flow simplification (your added goal).** So far only Grant credits is done (6 → 4 steps). Still to count and simplify, using the e2e flow table:
   - add a customer and get them billing (external ID auto-filled from the name, then a guided Create subscription step);
   - one-off invoice;
   - add a payment method;
   - find why a customer owes money.

   Each simplified flow gets a "before N → after M steps" line in its PR and the final report.
