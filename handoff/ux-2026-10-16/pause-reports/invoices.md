I've stopped as asked. Both branches are committed and pushed, and only one PR is open: #1093.

| Branch | Last commit | Pushed | PR |
|---|---|---|---|
| `ux/invoices-draft-seller` (base `feat/app-shell`) | `5b89d9f6` | yes | #1093 (https://github.com/getvelox/velox/pull/1093) |
| `ux/invoices-hosted-page` (stacked on draft-seller) | `3a954c6f` (WIP) | yes | none |

**#1093 (draft-seller)** is finished and passed every check except that `go test` ran without `-race`. It covers M-INV-1, M-INV-6, M-X-13 and the memo part of INV-15:
- Draft PDFs now carry a DRAFT mark and "Draft — not issued" instead of issue and due dates.
- The invoice and credit-note PDFs no longer print "Velox" when the company name is empty. The invoice page no longer shows "VELOX" or "Your Company".
- When seller details are missing, the invoice page shows a "Missing seller details → Settings" chip, and it now shows the memo.
- Seller rule I picked: warn at Finalize, never block. Cycle invoices are issued by the engine with no operator step, so a block could only ever apply to manual drafts.

**WIP hosted page (M-INV-2/3/14/15)** is coded but not ready:
- The server hides Pay and sends a new `pay_unavailable_reason` field when the tenant has no Stripe keys. A direct checkout request gets a 409 with code `online_payment_unavailable`.
- Customers no longer see server error text. Mobile Pay is full height, empty sections are hidden, and Subtotal/Total only show when they add something.
- The Go and web tests pass and both were mutation-checked; CHANGELOG and MANUAL_TEST are updated.
- One thing to know for the after screenshots: my first set ran against a stale API process, because the old server was still holding port 8183. I restarted it, and the API now returns `pay_enabled: false` with `online_payment_unavailable` for VLX-000002. I re-shot the after screenshots but haven't looked at them yet.

**Next steps to resume `ux/invoices-hosted-page`:**
1. Look at the new after screenshots in `/tmp/claude-0/ux/build-invoices/hosted/`.
2. Run the full checks: `/tmp/claude-0/ux/build-invoices/checks.sh <worktree> <out> ./internal/hostedinvoice/... ./internal/api/... -- src/pages/HostedInvoice.tsx src/lib/hostedInvoice.ts tests/hostedInvoice.test.ts`, then the integration tests for `hostedinvoice` with the lock.
3. Squash the WIP commit into a real commit and write the PR body to `/tmp/claude-0/ux/build-invoices/hosted-page.md`.
4. Open the PR with base `ux/invoices-draft-seller`.

**Rest of the chain, not started**, in this order:
1. Draft editing: PATCH/DELETE for draft lines on manual drafts only, plus the OpenAPI spec and `make gen`. Finalize confirmation showing totals, credits and due date, with the seller warning. Inline line editing, Email hidden on drafts, and one "Finalize & send" path, to cover the maintainer's simplify-flows goal.
2. "Discard draft" instead of a true delete. Invoice numbers are assigned when the draft is created, so a hard delete would leave a gap in the numbering. This needs your decision.
3. Uncollectible tab, and amount due / payment status for void and uncollectible invoices.
4. Attention "since" date and severity.
5. Server search, Customer filter and Issued/Due/Created date field.
6. Sidebar counts and row tones.
7. Record payment with date, method and reference.
8. PDF layout and unit-price display.
9. Tax row and the Manual-at-0% warning.
10. Dunning fixes and the decision on when retries start, with a new ADR (check ADR numbers across branches; 121 is taken).
11. One shared credit-note dialog, prefilled when opened from an invoice.
12. Redesigns of invoice detail, the invoices list and Dunning.
13. Count steps before and after for each simplified flow, using the flow table in `e2e.md`.

My worktree is `/home/user/velox/.claude/worktrees/inv-draft-seller`; I switch branches inside it rather than adding one per branch. My API on :8183 (from `/tmp/claude-0/inv-api`, restarted with `restart-api.sh`) and my vite on :5183 are still running.
