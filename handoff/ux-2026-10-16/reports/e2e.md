I ran all 8 flows end to end through the UI. All but a few steps could be finished; the exceptions are rows marked "not possible" or "broken" below. The worst problems are wrong or misleading numbers: the subscription invoice preview ignores proration, a canceled subscription says "Renews", the Dunning "Recovered" count includes voided and written-off invoices, and the invoice composer's "Unit ($)" column takes cents. Usage was sent through the API, as you allowed. Everything else was clicked and typed in Playwright, on records prefixed `ux-`. Seeded records were only read.

Evidence is in `/tmp/claude-0/ux/work-e2e/` (shown below as `E/`). The raw console and network log is `E/events.jsonl`.

To keep other auditors safe:
- **Shared session untouched by risky steps.** Sign-out, sign-in and the Test/Live toggle ran in a separate login, because the mode is stored on the server session. I checked afterwards that the shared `state.json` session still works and is still in Test mode.
- **Settings restored.** I changed company name, Net terms and timezone, then set them back to empty / Net 30 / Etc/UTC.
- **Webhook endpoint deleted.** I removed my `ux-` endpoint so it would not collect failing deliveries.

## A) Flow-by-flow

| Flow | Step | Result | Evidence | Notes (clicks/steps, exact UI text) |
|---|---|---|---|---|
| 1 Pricing | Create flat price | works | E/f1-01, f1-02, f1-03 | Prices tab → Add Price → 3 fields → Create (~6 steps). Submitting empty shows only "Price per unit: enter a price as a plain number, for example 0.01. Use 0 for free." Empty Name and Key are not flagged; there is one error box, not per-field errors. |
| 1 | Create graduated price | works | E/f1-04, f1-07, f1-08 | Switching to Graduated pre-fills 2 tiers with real values (100 @ 0.10, ∞ @ 0.05), not placeholders. |
| 1 | Create meter linked to a price | works | E/f1-09…f1-12 | Lands on the meter's page. Plan and Price creation stay on the list instead (inconsistent). The meter dialog's `<label>`s are not tied to their inputs. Key is not auto-filled from Name (same for plan code and customer external ID). |
| 1 | Create plan (base fee + meter) | works | E/f1-13…f1-15 | ~7 steps. Toast "Plan created". |
| 1 | Edit plan; detach/re-attach meter | works | E/f1-18…f1-23 | Detach asks "Detach meter?". Toasts "Meter detached", "Meter attached". |
| 1 | Archive or remove a price | **not possible** for archiving | E/f1-25…f1-29 | The UI and the API have no archive for a price. You can only unlink it from a meter, and that needs typing "remove". Archiving the plan works (E/f9-02, f9-03) with no confirmation. |
| 2 Customer | Create (with email) | works | E/f2-03; validation f2-26, f2-27 | Inline errors work: "Invalid email address"; a duplicate external ID shows the server message inline. |
| 2 | Edit customer | works | E/f2-06 | |
| 2 | Billing profile | works | E/f2-07…f2-10 | US switches State to a picker (good). The saved card shows "CURRENCY: Use tenant default" instead of USD. |
| 2 | Add payment method (no Stripe) | **broken** | E/f2-11, f2-12, f2-13 | The dialog never says Stripe isn't configured. "Send email" → toast "Failed to send setup link / Email isn't set up on this server…". "Copy link" → HTTP 500, toast "Something went wrong on the server… copy the error ID". The API log shows `ensure stripe customer: stripe not configured`. |
| 2 | Grant credits | works, with a defect | E/f2-19, f2-20b, f2-22 | Customer → Credit Balance link → /credits → Grant → confirm (~6 steps). Before the first grant the header reads "Customer", not the name (`Credits.tsx:238,274`). |
| 2 | Price override | works | E/f2-14…f2-16 | No success toast (`PriceOverridesCard.tsx` has none). |
| 2 | Cost-dashboard URL | **confusing** | E/f2-17, f2-18-* | The first click's toast says "Cost-dashboard URL rotated". The result is a bare path, and the UI warns it can't be opened as-is. Opening it returns raw JSON (it includes tenant_id and a `"0001-01-01"` period), not a page. |
| 2 | Archive customer | works | E/f2-23, f2-24 | Clear confirm text; archived banner offers "Restore customer". |
| 3 Subscription | Create with 14-day trial | works | E/f3-02-*, f3-04 | ~8 steps. Code is not auto-filled. |
| 3 | Create a second sub on the same customer | blocked by design | E/f3-03-sub-main-created | The error shows the raw ID: "already bills meter vlx_mtr_db905f…". |
| 3 | Extend trial | works | E/f3-05, f3-06 | Toast "Trial extended / The trial now ends Nov 6…". |
| 3 | Schedule cancel at trial end, then Undo | works | E/f3-08, f3-10 | While scheduled, the banner says "the customer won't be charged" but the timeline still shows "First charge Dec 1, 2026". |
| 3 | Cancel immediately | works, wrong labels after | E/f3-13, f3-14 | Two dialogs plus typing "CANCEL", next to a dismiss button also labelled "Cancel". Afterwards the timeline says "Canceled at trial end" and the summary says "Renews Dec 1, 2026" on a canceled sub. |
| 3 | Create sub without trial | works | E/f3-15-*, f3-16 | |
| 3 | Change quantity 1→3 | works, inconsistent copy | E/f3-17, f3-18 | The dialog says "Takes effect now"; the Activity feed says "To qty 3 · At next period". The preview bills the full period at qty 3 ($117). |
| 3 | Change plan; cancel the pending change | works | E/f3-19…f3-24 | Toast "Plan change scheduled / The new plan starts Nov 1, 2026." Canceling the pending change needs no confirmation. |
| 3 | Add item (EUR plan → USD sub) | blocked, but picker offers it | E/f3-26 | Toast "Plan "ux-plan" is in USD but plan "pro-eur" (EUR) is in EUR…". |
| 3 | Add / remove item | works, preview wrong | E/f3-27…f3-30 | The dialog says the item is billed only for the rest of the period; the preview shows the full $15.00. |
| 3 | Pause / resume collection | works | E/f3-31, f3-32, f3-34 | Status badge stays "active" while paused (a banner explains it). Resume has no confirmation. |
| 3 | Schedule cancel at period end, then Undo | works | E/f3-35…f3-37 | "Period End Oct 31" and "Cancels Nov 1, 2026, 12:00 AM UTC" appear on the same page. |
| 4 Usage | Ingest 3 events (API) | works | — | 400 + 800 + 1200 = 2,400 calls. |
| 4 | Usage events page | works | E/f4-01 | The "Total Units 842,540" card adds tokens, calls and GB together. |
| 4 | Customer Activity card | works | E/f4-02 | Shows $17.00 billed usage, but defaults to the 30d tab, not Cycle. Upcoming card reads "Day 0 of 16 · 0%". |
| 4 | Subscription view | works | E/f4-03 | Preview line "ux-calls 2,400 $17.00" is list price. The override card says "since Oct 16" but its $0.004 rate isn't used; the dialog does say "from next period". |
| 4 | Filter by customer / meter / date | works | E/f4-04…f4-07 | Filters are kept in the URL (good). The customer search also lists the archived customer. |
| 4 | Export CSV | works | E/dl-usage-usage-events.csv | Download starts and respects filters. No event ID, customer ID or ISO timestamp in the file. |
| 5 Invoices | One-off invoice from customer page | works, but **confusing units** | E/f5-01…f5-03 | The column says "UNIT ($)" but the help text says "Amounts are in cents (e.g. 2500 = $25.00)". |
| 5 | Edit draft: add line | works | E/f5-05, f5-08 | This dialog's "Unit Price ($)" takes dollars (placeholder 10.00), the opposite of the composer. |
| 5 | Edit draft: remove line | **not possible** | E/f5-09 | No remove or edit control, and no API endpoint for it. The only fix is void and recreate. |
| 5 | Finalize | works, **no confirmation** | E/f5-11, f5-12 | One click, irreversible (`InvoiceDetail.tsx:745`). Credits ($25) were applied silently. |
| 5 | Download PDF | works | E/dl-VLX-000008.pdf | The PDF has the billing address and memo; the on-screen invoice shows neither. On-screen FROM reads "Your Company", while the PDF header says "Velox". |
| 5 | Credit note ($5) | works | E/f5-14, f5-16 | No confirmation. The Activity "Invoice finalized" amount changed from $205 to $200 afterwards. |
| 5 | Record offline payment | works | E/f5-17, f5-18 | Full amount only, no partial payment. The reference "ux wire 001" isn't shown in Activity. |
| 5 | Mark uncollectible | works | E/f5-23, f5-24 | Requires typing "WRITE OFF". |
| 5 | Void | works | E/f5-21, f5-22 | Requires typing "VOID". The detail page shows Amount Due $0.00; the list and CSV show $15.00. |
| 5 | Save & send (no SMTP) | works, honest | E/f5-19 | "Invoice VLX-000009 finalized, email not sent / Email isn't set up on this server…". |
| 5 | List tabs and filters | **broken for uncollectible** | E/f5-26-* | VLX-000010 (uncollectible) is in no tab except All. Search matches invoice number only, not customer. |
| 5 | Export invoices CSV | works | E/dl-inv-invoices-20261016-111429.csv | Voided and uncollectible rows show payment_status "pending". |
| 6 Test clocks | Create clock; customer on it; subscribe | works | E/f6-03…f6-07 | |
| 6 | Advance +1mo (twice) | works | E/f6-08, f6-09-1, f6-09-2, f6-10…f6-13 | Generated VLX-000011 ($20.13, prorated 16/31) and VLX-000012 (a draft, because dunning paused collection). The clock page then shows a stale "Next bill: Nov 1" / "Dec 1" while the clock is at Nov 16 / Dec 16. The invoice says "Payment recovery ended · since 45d ago"; recovery ended Nov 15, 31 days earlier. |
| 7 Admin | Create API key; secret shown once; revoke | works | E/f7-01…f7-05 | Checked via curl: 200 before revoke, 401 after. Esc closes the one-time secret dialog with no warning. A 30-day key shows "Expires in 31d". |
| 7 | Webhook endpoint | works | E/f7-08-*, f7-09 | `http://127.0.0.1:9` was accepted. `https://ux-hook.invalid` gets inline "can't be reached: no address was found…". |
| 7 | Send test event | **not possible** | E/f7-10 | No test-event control in UI or API. I fired a real event by finalizing an invoice: "connection refused" is visible when a row is expanded (E/f7-13). Replay works (E/f7-14). |
| 7 | View webhook events | works | E/f7-12 | Raw status `no_endpoints`. No filters. |
| 7 | Invite member | blocked, honest | E/f7-24, f7-25 | "Invites are off — This server isn't set up to email invite links." The form is disabled. There is no copy-link fallback. |
| 7 | Settings: company name, Net terms, timezone | saves, partly ignored | E/f7-19, f7-20, f7-21, f7-22 | Timezone applied everywhere (EDT shown). Company name shows on drafts; already-finalized invoices keep their snapshot, which is correct. Net 15 is **ignored** by the invoice composer, which still defaults to Net 30 (`CustomerDetail.tsx:1802`). |
| 7 | Audit log | works | E/f7-23, f7-39, f7-40 | Archiving a customer is logged as "Updated ux-cust-archive". Rotating the cost-dashboard token is logged as "Rotated customer". |
| 7 | Test/Live toggle | works | E/f7-28…f7-30 | Live mode shows the banner "LIVE mode but no Stripe live credentials — real charges will fail." |
| 7 | Ctrl+K search | works | E/f7-31…f7-33 | Finds the customer, invoice and plan; Enter navigates. The invoice result shows "paid -- $0.00" (amount due, not total). |
| 7 | Sign out / sign in | works | E/f7-26, f7-35, f7-36 | Wrong password shows "Invalid email or password". Re-login returns to the page you came from. |
| 7 | Forgot password | blocked, honest | E/f7-38 | "Password reset email is off". |
| 8 Dunning | Runs list | works, **wrong count** | E/f8-01, f8-03 | "Recovered 3" and the Recovered tab include "Written off" and "Voided". Dunning starts immediately on a Net 30 invoice ("Next retry in 2d"). |
| 8 | Create / edit / restore policy | works | E/f8-05…f8-08 | I used a `ux-` policy, not the default. An invalid schedule shows only a toast, never inline ("Entry 0 ("1 day") is not a valid Go duration…"). The hint "Need 2 entries for 3 retries" contradicts the default policy, which shows 4 entries for 4 retries. Its 336h step never ran on the clock. |

## B) Findings

| id | page | severity | category | evidence | recommendation | size |
|---|---|---|---|---|---|---|
| E2E-1 | Subscription detail | broken | function | E/f3-16, f3-18, f3-28 vs E/f6-13 ($20.13 actually billed) | Make Next Invoice Preview call the same proration path as billing, and label prorated lines like the real invoice does ("prorated 16/31 days"). | M |
| E2E-2 | Subscription detail (canceled) | broken | function | E/f3-14; `SubscriptionDetail.tsx:767-776` and `:586` | Add a `status==='canceled'` branch ("Canceled on <date>", no period). Use "Canceled during trial" when canceled before trial end. | S |
| E2E-3 | Invoice composer | broken | function | E/f5-02; `CustomerDetail.tsx` composer vs the Add Line Item dialog on invoice detail | Take dollars in both places (like Stripe's invoice editor) and convert to cents in code. | S |
| E2E-4 | Invoice detail (draft) | broken | function | E/f5-09; no DELETE/PATCH on line-items in openapi | Add edit/delete for draft lines (Stripe-style inline editing while draft). | M |
| E2E-5 | Invoice detail | confusing | interaction | E/f5-11; `InvoiceDetail.tsx:745` | Confirm before finalizing; show the total, credits to apply and due date. | S |
| E2E-6 | Add payment method | broken | function | E/f2-13; API log `stripe not configured` → 500 | Map to a 409 with a clear message. Disable the dialog with "Connect Stripe first" when no keys are set. | S |
| E2E-7 | Invoices list | broken | function | E/f5-26-* | Add an "Uncollectible" tab, or fold it into Open/Past due with a badge. | S |
| E2E-8 | Dunning | confusing | function | E/f8-01, f8-03 | Count only payment-recovered runs as "Recovered". Add a "Closed (voided/written off)" bucket. | S |
| E2E-9 | Invoices list / CSV | confusing | consistency | E/f5-22 vs E/f5-26-Voided | Show $0 / "—" amount due for voided invoices in list and CSV, and a payment_status other than "pending". | S |
| E2E-10 | Test clock detail | broken | function | E/f6-09-1, f6-09-2 | Take "Next bill" from the sub's current_billing_period_end after an advance; refetch after the advance finishes. | S |
| E2E-11 | Invoice detail (dunning banner) | confusing | function | E/f6-13 | "since 45d ago" should count from when recovery ended (31d). | S |
| E2E-12 | Settings → composer | broken | function | E/f7-22; `CustomerDetail.tsx:1802` | Default `paymentTermDays` to the tenant's `net_payment_terms`. | S |
| E2E-13 | Subscription (scheduled cancel) | confusing | consistency | E/f3-08 | Hide "First charge" when a cancel is scheduled before it. | S |
| E2E-14 | Subscription (period end) | confusing | consistency | E/f3-36 | Pick one convention: "Period End Oct 31" vs "Cancels Nov 1, 12:00 AM". Stripe shows "Cancels Oct 31 end of day". | S |
| E2E-15 | Change Qty | confusing | function | E/f3-17, f3-18 | Align the dialog copy, the Activity entry ("At next period") and the preview. | S |
| E2E-16 | Public cost dashboard | confusing | function | E/f2-17, f2-18-api | Ship a hosted page (like Orb/Metronome embeddable usage pages), or call it "API endpoint". Show a full URL. Don't expose tenant_id or zero dates. Say "generated" on first use. | M |
| E2E-17 | Pricing | friction | function | E/f1-25…f1-29 | Add archive/unarchive for prices (Stripe "Archive price"). | M |
| E2E-18 | Credits | polish | function | E/f2-19; `Credits.tsx:238,274` | Load the customer name even when the customer has no balance yet. | S |
| E2E-19 | Typed confirmations | friction | consistency | f1-28 "remove", f3-13 "CANCEL", f5-21 "VOID", f5-23 "WRITE OFF", f7-42 "DELETE" | One convention for the word, its case and its placement. Avoid a "Cancel" dismiss next to "Cancel subscription" (use "Keep subscription", as the first dialog already does). | S |
| E2E-20 | Subscription create / add item | friction | interaction | E/f3-03-*, f3-26 | Filter out plans in a different currency (as already done for interval). Show the meter name, not its ID, in the double-billing error. | S |
| E2E-21 | Usage events | confusing | function | E/f4-01 | Drop "Total Units" across meters, or show totals per unit. | S |
| E2E-22 | Usage CSV | polish | function | E/dl-usage-usage-events.csv | Add event_id, customer external_id and ISO timestamps. | S |
| E2E-23 | Invoice detail vs PDF | confusing | consistency | E/f5-04 vs dl-VLX-000008.pdf | Show Bill-To address and memo on screen. Use one placeholder for an empty seller name ("Your Company" vs "Velox"). | S |
| E2E-24 | Invoice Activity | polish | function | E/f5-16, f5-18 | Show the amount at the time of each event; show the offline-payment reference. | S |
| E2E-25 | Record payment | friction | function | E/f5-17 | Allow partial amounts. | M |
| E2E-26 | Price override | polish | interaction | E/f2-16 | Add a success toast like other mutations. Make clear that the override only applies from next period. | S |
| E2E-27 | Customer billing profile | polish | function | E/f2-10 | Show the resolved currency ("USD (tenant default)"). | S |
| E2E-28 | API keys | friction | interaction | E/f7-02, f7-03, f7-05 | Block Esc and outside-click on the one-time secret until "I've saved this key". Fix the "Expires in 31d" off-by-one. | S |
| E2E-29 | Webhooks | friction | function | E/f7-10, f7-12 | Add "Send test event" (Stripe/Resend pattern). Add event-type and customer filters. Humanize `no_endpoints`. | M |
| E2E-30 | Team invite | friction | function | E/f7-25 | Offer "copy invite link" when SMTP is off, as the payment-method dialog does. | S |
| E2E-31 | Audit log | polish | function | E/f7-39 | Log archive/restore as their own actions. "Rotated customer" → "Rotated cost-dashboard URL". | S |
| E2E-32 | Dunning policies | confusing | function | E/f8-04, f8-05; default policy shows 4 intervals for 4 retries | Validate inline. Make the schedule hint and the stored default agree, and warn when there are surplus intervals. Show human durations ("3 days"). | S |
| E2E-33 | Dunning | confusing | function | E/f8-01 ("Next retry in 2d" on a Net 30 invoice) | Start retries at the due date, or label them "reminders". | M |
| E2E-34 | Ctrl+K | polish | consistency | E/f7-33 | Show the invoice total, and "—" instead of "--". | S |
| E2E-35 | Customer Activity / Upcoming | polish | function | E/f4-02 | Default to the Cycle tab when a subscription exists. "Day 1 of 16", not "Day 0". | S |
| E2E-36 | Pricing dialogs | polish | a11y / consistency | E/f1-02, f1-09 | Show field errors next to their fields. Tie the meter dialog's labels to their inputs. After creating, land consistently (all on the detail page or all on the list). | S |
| E2E-37 | Subscription (paused) | polish | visual | E/f3-32 | Add a "collection paused" status badge. | S |
| E2E-38 | Billing profile fetch | polish | function | `events.jsonl`: 11× 404 /billing-profile | Return 200 with null for "no profile" instead of 404, which logs a console error on every visit. | S |

## C) Console errors and failed requests
There were no JS exceptions (`pageerror`). The only console errors were "Failed to load resource" lines for these responses:
- 11× `GET /v1/customers/{id}/billing-profile` 404 "billing profile not found" (normal empty state, but logged as an error)
- 6× `GET /v1/whoami` 401 on the login page (expected when signed out)
- `POST /v1/customers/{id}/payment-methods/setup-session` **500** "an internal error occurred" (Stripe not configured)
- `POST …/send-setup-email` 409 email_not_configured; `POST /v1/invoices/{id}/send` 409 (same)
- `POST /v1/customers` 409 duplicate external_id (handled inline)
- `POST /v1/subscriptions` 409 meter already billed; `POST /v1/subscriptions/{id}/items` 409 currency mismatch
- `POST /v1/webhook-endpoints/endpoints` 422 DNS failure (handled inline)
- `POST /v1/dunning/policies` 422 invalid duration (toast only)
- `POST /v1/auth/login` 401 (wrong password, handled)
- 2× `net::ERR_ABORTED` (requests cancelled by navigation)

`/webhooks/events` never reaches network-idle (it is a live tail). That is harmless for users.

## D) What works smoothly
- Every message about missing email or Stripe is honest except the setup-link 500: Save & send, invites, forgot password, and the Live-mode banner.
- Guardrails catch real mistakes and explain the fix: double-billing a meter, mixed currency, plan in use, dangerous actions with clear consequences.
- Test clocks work end to end: proration on the first invoice, dunning progression, "Last advance results", and a "Simulated time" banner on every affected page.
- URL-persisted filters on Usage, Invoices and Audit log; CSV exports download immediately.
- Credits flow: confirm step, ledger with running balance, auto-applied on finalize, shown on the invoice and PDF.
- Credit notes reflect immediately on the invoice, the credit-notes list and the webhook events.
- Ctrl+K finds customers, invoices and plans. Sign-out and sign-in work and keep deep links. The audit log records almost every action with the right actor.

## Records I left behind
- `ux-cust-one-edited`: active, with sub `ux-sub-main` (qty 3), invoices VLX-000008 to 000010 and 000013, a $5 credit note, and a price override.
- `ux-cust-archive`: archived.
- `ux-clock` with `ux-clock-cust` / `ux-clock-sub`, and invoices VLX-000011 and 000012.
- Plan `ux-plan-edited` (now archived), meter `ux-calls`, prices `ux_flat_calls` and `ux_grad_calls`, and `ux-policy`.
