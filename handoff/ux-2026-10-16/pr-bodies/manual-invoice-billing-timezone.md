## What & why

**Manual invoices now store the tenant billing timezone, so their PDF and hosted page print dates in that zone.** `POST /v1/invoices` (`invoice.Service.Create`) never set `billing_timezone`. Every engine writer stamps it (ADR-077). The PDF takes its document zone from that column (`pdf.go` ~446, `LoadLocationOrUTC("")` = UTC). So on a manual invoice:

- Issued, Due, Paid and Void rows on the PDF (`pdf.go` ~449-453, ~807, ~812) were in UTC.
- The Period row (`pdf_context.go` ~76-79) and the API `billing_period_display` (`service.go` ~969-977) fell back to the live tenant zone.
- The public hosted page (`HostedInvoice.tsx`, `billing_timezone || "UTC"`), the dunning email retry date (`dunning/service.go` ~1610) and credit-note PDFs (`creditnote/pdf.go` ~133) were in UTC.

Scenario: America/Los_Angeles tenant finalizes a manual invoice at 2026-10-16 18:00 PDT (`2026-10-17T01:00Z`).

| | `billing_timezone` | Dashboard | PDF |
|---|---|---|---|
| Before | `""` | Issued Oct 16 | "Issued October 17, 2026" |
| After | `America/Los_Angeles` | Issued Oct 16 | "Issued October 16, 2026" |

Change:

- `Create` reads tenant settings once, for the net-terms default and the timezone. It stamps `BillingTimezone: domain.LoadLocationOrUTC(ts.Timezone).String()`. With no zone set, or no settings reader wired, it stamps `"UTC"`, as the engine does.
- A settings read failure fails the create before an invoice number is spent. That was already true when Net terms were omitted (P24). **It now also applies when explicit Net terms are sent**, because the zone is needed. The alternative was to stamp UTC on a read blip, which would write a wrong durable value.
- The `domain.Invoice.BillingTimezone` comment and the OpenAPI `billing_timezone` description said "empty for ad-hoc invoices". Both now say empty only on legacy rows. `make gen` drift is the description only (`api.gen.go`, `web-v2/public/openapi.yaml`, `api.gen.ts`, `gen/schemas/invoice.ts`).

**Not done, on purpose:** no backfill. Manual invoices created before this change keep an empty `billing_timezone` and still render UTC on the PDF and hosted page. This matches pre-0134 engine rows, which were deliberately not backfilled. Drafts created before the fix and finalized after it also stay empty; `Finalize` does not stamp.

**Overlap with `fix/due-date-tenant-calendar`:** both branches touch `invoice.Service.Create`. They change different lines, and `git merge-tree` conflicts only in `CHANGELOG.md`. After both merge, `Create`'s due date can use this branch's `billingLoc` instead of a second `invoiceLocation` read. That is optional cleanup; the two resolve the same zone.

Sibling writers checked (every `domain.Invoice{` construction with fields): `billing/engine.go` ~3477/3887/4624, `billing/threshold_scan.go` ~792 and `subscription/handler.go` ~2488 already stamp `billingLoc.String()`. `invoice/service.go` ~606 (manual create) was the only one without it. `cmd/velox-migrate-safety/seed.go` is a test seeder, not a product path.

### Tests

`internal/invoice/manual_billing_timezone_test.go`:

- `TestManualInvoice_StampsTenantBillingTimezone`: the scenario above. It asserts the zone on create and finalize, and the PDF's Issued day computed with the same expression `pdf.go` uses.
- `TestManualInvoice_NoTenantTimezoneStampsUTC`: no configured zone stamps `"UTC"`.
- `TestManualInvoice_UnreadableSettingsFailsCreateWithExplicitTerms`: a read failure fails the create, and no invoice number is spent.

Mutation check (made the stamp return `""`, ran, restored):

```
manual_billing_timezone_test.go:40: create billing_timezone = "", want "America/Los_Angeles"
manual_billing_timezone_test.go:67: billing_timezone = "", want "UTC"
```

### Checks run

- `gofmt -l internal/ cmd/`: empty.
- `go vet ./internal/domain/... ./internal/invoice/... ./internal/api/...`: ok.
- `golangci-lint run` (v2.12.2, the CI pin, built with go1.26) on the same packages: `0 issues.`
- `make lint-clock lint-tz lint-manual-test lint-funding-set`: all ok.
- `make gen`: only the expected description drift, committed.
- web-v2: `npx tsc -b` ok, `npx eslint` on the 2 generated files ok, `npm test` 233/233 pass.
- `go test ./... -short -count=1`: all ok.
- `go test -p 1 -count=1 -short=false` on domain, invoice and api: domain and invoice pass. **api: 1 failure, `TestE2E_ReferencedIDsAreTenantAndModeScoped`.** It fails on a webhook-endpoint seed with `can't be reached: no address was found for example.com`, because this sandbox has no DNS. The same test fails the same way on `fix/due-date-tenant-calendar`, which does not touch that code. Every other api test passes.

### Screens to look at (no browser was available)

1. Settings → timezone `America/Los_Angeles`. Compose and finalize a one-off invoice after 17:00 PDT (after 00:00 UTC). On the invoice detail page, Issued shows today's date. Download the PDF: **Issued** and **Due Date** show the same days as the dashboard.
2. Open the same invoice's hosted link (`/invoice/{token}`) from a browser in another zone. The Due date matches the PDF.
3. Control: open a manual invoice created before this change. Its PDF still shows UTC dates (not backfilled).

## Checklist

- [x] Tests cover the new behavior (unit; integration too if a store or money path changed)
- [ ] `go test ./... -race -short -count=1` passes (ran without `-race`; passes)
- [x] `make lint` passes (golangci-lint v2.12.2 on touched packages: 0 issues)
- [x] `CHANGELOG.md` entry added — required for any user-visible change
- [x] `MANUAL_TEST.md` flow added/updated — FLOW TZ1, new `[ ]` box after the hosted-invoice dates box (the PDF render itself is not walked)
- [x] `make gen` re-run — required if `api/openapi.yaml` changed
- [x] Frontend touched? Generated files only; `tsc -b`, eslint, `npm test` pass

## Money path only

State: `invoices.billing_timezone`. It is immutable after insert and is the zone for every customer-facing date. Site-set from
`grep -rn "BillingTimezone\|domain.Invoice{\|INSERT INTO invoices" internal cmd --include=*.go`:

**Writers:**

- [x] `invoice/service.go` `Create` (manual): **now stamps the zone**.
- [x] `billing/engine.go` cycle close (~3480), BillOnCreate (~3891), final-on-cancel (~4628): already stamp `billingLoc.String()`.
- [x] `billing/threshold_scan.go` (~795): already stamps it.
- [x] `subscription/handler.go` proration invoice (~2508): already stamps it.
- [x] Store: `invoice/postgres.go` insert (~343) and Tx insert (~2126) persist the field. No UPDATE writes it.

**Gated readers (read the field; empty falls back as before):**

- `invoice/pdf.go` ~446 (UTC fallback)
- `invoice/pdf_context.go` ~77 (tenant fallback)
- `invoice/service.go` `invoiceDisplayLoc` ~974 (tenant fallback)
- `invoice/postgres.go` ~154 (duplicate-period message)
- `hostedinvoice/handler.go` ~487
- `creditnote/handler.go` ~365 and `creditnote/pdf.go` ~133
- `dunning/service.go` ~1610

New manual rows now take the stamped branch in each.

**Effect-firers / crash points:** none added. The value is set on the struct before the existing create transaction. The settings read happens before the invoice number is minted.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
