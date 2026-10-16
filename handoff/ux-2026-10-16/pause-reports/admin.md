Paused as asked. Both branches are committed and pushed, and I've stopped my API on :8185 and my vite on :5185.

**PRs opened:** one, #1094 (https://github.com/getvelox/velox/pull/1094). No PR for the paused WIP branch.

| Branch | Last commit | Pushed | PR | Base |
|---|---|---|---|---|
| `ux/admin-home-numbers` | `2c5dc144` | yes | #1094 | `feat/app-shell` |
| `ux/admin-email-settings` | `471e1711` (WIP) | yes | none | `ux/admin-home-numbers` |

### `ux/admin-home-numbers` (#1094, finished)
- **What it fixes:** M-HOME-1/2/3 and NEW-1/NEW-2.
  - The overview API gains `live_customers`, `live_customers_prev` and `failed_payments_prev`.
  - `trialing_subscriptions` now counts subscriptions with status `trialing`. Before, it always read 0.
  - Home compares each trend with these server baselines. The card is renamed "Customers" (customers with an active or trialing subscription), and the MRR sparkline that was drawn from revenue is gone.
- **Tests:** a real-Postgres test, `TestOverview_HomeCountsAndBaselines`, plus `web-v2/tests/homeKpis.test.ts`. I reverted each fix in turn and the matching test failed every time.
- **Checks:** all pass.
- **Screenshots:** in `/tmp/claude-0/ux/build-admin/home-numbers/`. I looked at them. Before, the Customers card read "7 ↑800%"; after, it reads "Customers 5 · 4 active · 1 trialing subscriptions".
- **To resume:** nothing left on this branch. Rebase it if #1084 or #1071 merge first, since all three touch `Dashboard.tsx`.

### `ux/admin-email-settings` (WIP, not ready)
- **Done (M-X-14):**
  - `GET /v1/settings/email` shows the SMTP status and which email-link env vars are set.
  - `POST /v1/settings/email/test` sends a test email over SMTP right away. It goes only to the signed-in member's own address, works only from a dashboard session, and writes an audit row.
  - whoami now returns `smtp_configured`, which the invoices area can read to show one global "email is off" notice.
  - OpenAPI is updated and `make gen` re-run.
  - Settings has a new Email tab with a test-send button. Team's "Invites are off" notice now links to it.
  - On phones, the settings tab strip scrolls instead of widening the page to 524px.
  - Go handler tests and `web-v2/tests/emailSettings.test.ts` pass, and the audit route registry test passes.
- **To resume:**
  1. Screenshot the email-configured state. There is no mail server here, so this needs a small local SMTP sink. Then screenshot a successful test send and a refused one (502).
  2. Re-take the 390px shot with the scrolling tab strip and look at it.
  3. Mutation-check the new tests and record the failing lines.
  4. Add the CHANGELOG entry (Added, with an "API:" note) and a MANUAL_TEST box. Run the full checks with `/tmp/claude-0/ux/build-admin/checks.sh`.
  5. Rewrite the commit message (it is a WIP commit), push, and open the PR against `ux/admin-home-numbers`.

### Not started
These remain in the planned order:
- Webhooks: M-WH-1/2/3 and M-ADM-1, including a "Send test event" endpoint.
- API keys: M-API-1 and M-ADM-3.
- Invite copy-link fallback: M-ADM-2.
- Small fixes: M-SET-1, M-SHELL-8, M-ADM-4 and M-X-15.
- Redesigns: Home (merging fix/revenue-chart-zero-fill and fix/chart-day-labels first), Settings with a left sub-nav, the API keys / Webhooks / Audit log tables, and Login / Not found.

None of the flow step counts the maintainer asked for are done yet. They need the before numbers from the e2e.md flow table, and each "before N steps → after M steps" goes in that flow's PR body.

### Helpers for resuming
All in `/tmp/claude-0/ux/build-admin/`:
- **`serve.sh <worktree>`:** builds the API from a worktree and runs it on :8185, with vite on :5185 pointed at it.
- **`shoot.cjs`:** takes light, dark and 390px screenshots.
- **`checks.sh`:** runs the full check list.
