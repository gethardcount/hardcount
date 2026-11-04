package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/api/timefilter"
	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/usage"
)

// Data export: streaming CSV endpoints, so a tenant can take its data out.
// Procurement reviews ask for this ("can I take my data?"). Each export is
// scoped by RLS through the standard Bearer-key auth chain.
//
// Each endpoint runs its own query (StreamForExport on its store), not the
// store's List method. List's SELECT does not carry two columns the CSVs
// promise, customers.email_status and usage_events.origin. So an export built
// on List would leave them blank. An export's columns are its own contract.
//
// Streaming: one snapshot transaction per export, and one query. Rows go to
// the csv.Writer as they are scanned, with a flush every exportPageSize rows.
// So the HTTP buffer never holds the full result, and operators can export
// millions of rows without running out of memory.
//
// The single snapshot is what makes an export correct, not an optimization.
// LIMIT/OFFSET pages over a newest-first order, each in its own transaction,
// would duplicate and skip rows. A row inserted during such an export would
// enter at the head and shift every later row down by one. Each
// page-boundary row would then be written twice, and a delete would skip
// one. The usage export has a
// five-minute timeout because it streams many rows, and concurrent ingest is
// certain in that window. A duplicated usage row in a finance CSV reads as
// usage that did not happen. Under one MVCC snapshot, this cannot arise
// (#475).
//
// Date filters: the four data exports accept ?from=<RFC3339>&to=<RFC3339>.
// usage_events requires both, because the table has no bound: without a
// range, an export would walk the entire history.
//
// The audit-log export is the exception. It takes ?date_from and ?date_to,
// the params of the audit-log read route, so the dashboard's on-screen
// filters and its Export select the same rows. Passing ?from and ?to there
// is silently ignored, and you get the whole log. This trap is written down
// on purpose, since the two routes sit side by side in the same handler.
//
// Audit (ADR-090 §7): every export here emits an action=export audit row
// before the first byte streams, and fails closed if it cannot. This is the
// only read path in Hardcount that writes to audit_log. See auditExport for
// why the order matters.

const (
	// exportTimeout is how long an export may run: the /v1/exports routes'
	// request timeout (router.go). An export streams for as long as its
	// query does, far past the 30 s the other routes get.
	exportTimeout = 5 * time.Minute

	// exportWriteGrace is how far an export's write deadline reaches past
	// exportTimeout. An export that runs out of time writes the
	// EXPORT_INCOMPLETE marker, and the grace lets that marker still reach
	// the socket. It is the margin the server's WriteTimeout keeps over the
	// 30 s request timeout (cmd/hardcount/main.go).
	exportWriteGrace = 5 * time.Second

	// exportPageSize is the flush interval: the rows written to the
	// csv.Writer between flushes to the wire. Despite its name, it is not a
	// page size, because nothing pages. Every export (audit-log.csv too)
	// walks one snapshot cursor. So this number only trades HTTP buffer size
	// against syscalls, and no correctness depends on it.
	exportPageSize = 100

	// usageExportMaxSpanDays caps the date range for usage_events
	// exports. Larger spans should be split into multiple calls so a
	// single export doesn't lock the table.
	usageExportMaxSpanDays = 366
)

// exportAuditor writes the export's own audit row, on its own transaction.
// An export is a read, so there is no business transaction for ADR-090's
// LogInTx to join. *audit.Logger satisfies it. Its Log detaches from the
// caller's cancellation, which is the property this path needs.
type exportAuditor interface {
	Log(ctx context.Context, tenantID, action, resourceType, resourceID, resourceLabel string, metadata map[string]any) error
}

// auditStreamer reads the audit log itself, unbounded, for the audit-log CSV
// export. *audit.Logger satisfies it.
type auditStreamer interface {
	Stream(ctx context.Context, tenantID string, filter audit.QueryFilter, fn func(domain.AuditEntry) error) error
}

type exportsHandler struct {
	customers     *customer.PostgresStore
	invoices      *invoice.PostgresStore
	subscriptions *subscription.PostgresStore
	usage         *usage.PostgresStore
	audit         exportAuditor
	auditLog      auditStreamer
}

func newExportsHandler(
	c *customer.PostgresStore,
	i *invoice.PostgresStore,
	s *subscription.PostgresStore,
	u *usage.PostgresStore,
	auditor exportAuditor,
	auditLog auditStreamer,
) *exportsHandler {
	return &exportsHandler{
		customers:     c,
		invoices:      i,
		subscriptions: s,
		usage:         u,
		audit:         auditor,
		auditLog:      auditLog,
	}
}

func (h *exportsHandler) Routes(customerRead, invoiceRead, subscriptionRead, usageRead, auditRead func(http.Handler) http.Handler) chi.Router {
	r := chi.NewRouter()
	r.With(customerRead).Get("/customers.csv", h.exportCustomers)
	r.With(invoiceRead).Get("/invoices.csv", h.exportInvoices)
	r.With(subscriptionRead).Get("/subscriptions.csv", h.exportSubscriptions)
	r.With(usageRead).Get("/usage-events.csv", h.exportUsageEvents)
	// The audit log can be exported too. It needs the same permission as the
	// audit-log read route: auth.PermAPIKeyRead (see the /v1/audit-log mount
	// in router.go). The file is the same data. A different gate would either
	// lock out operators who can already read it on screen, or give it to
	// operators who cannot.
	r.With(auditRead).Get("/audit-log.csv", h.exportAuditLog)
	return r
}

// parseDateRange reads ?from and ?to from the request. It wraps
// timefilter.ParseRange, so exports share the date-filter contract of the
// audit-log and usage queries: RFC3339 or YYYY-MM-DD, both accepted
// everywhere. A missing value is a zero time.Time.
//
// Each store's StreamForExport applies the range in SQL. Nothing filters rows
// after loading them, so a filtered export does not walk every row in the
// tenant.
func parseDateRange(r *http.Request) (from, to time.Time, err error) {
	return timefilter.ParseRange(r, "from", "to")
}

// exportFilename returns the download name, with a timestamp so a re-export
// does not overwrite an earlier file. It is built before the audit row, so
// the row records the exact filename the operator received. That string is
// how a file found later is traced back to the export that produced it.
func exportFilename(stem string) string {
	// The stamp must be in UTC, so it is unambiguous and does not vary with
	// the operator's time zone.
	return fmt.Sprintf("%s-%s.csv", stem, time.Now().UTC().Format("20060102-150405")) //tz:ok
}

// writeCSVHeaders sets the response headers for a CSV download and gives the
// response the write deadline an export needs (extendExportWriteDeadline).
func writeCSVHeaders(w http.ResponseWriter, r *http.Request, filename string) *csv.Writer {
	extendExportWriteDeadline(w, r)
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename=%q`, filename))
	w.Header().Set("Cache-Control", "no-store")
	return csv.NewWriter(w)
}

// extendExportWriteDeadline moves this response's write deadline to
// exportTimeout + exportWriteGrace from now. The server's WriteTimeout (35 s,
// cmd/hardcount/main.go) bounds every response's writes. Without this call,
// an export would be cut at 35 s, mid-file, although its route allows five
// minutes. The SSE stream lifts the same deadline (webhook sse.go). The call
// reaches the connection through the middleware's writers, which all
// Unwrap. A writer that cannot take a deadline is logged. Such an export
// still works, but is cut at the server's WriteTimeout.
func extendExportWriteDeadline(w http.ResponseWriter, r *http.Request) {
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(exportTimeout + exportWriteGrace)); err != nil {
		slog.WarnContext(r.Context(), "csv export: could not extend the write deadline — an export longer than the server's WriteTimeout will be cut off",
			"error", err)
	}
}

// auditExport records a bulk data egress. It is the gate an export must pass
// before it streams a single byte.
//
// The order is the point, and it is why this is not a defer:
//
//   - Emit, then stream: this can only over-record. An export that is audited
//     and then fails mid-file leaves a row for a file the operator never fully
//     got. A reader can reconcile that against the EXPORT_INCOMPLETE marker.
//   - Stream, then emit: this under-records, and that cannot be undone. If
//     the connection dies mid-stream, a 5-minute timeout fires or the
//     process restarts, pages of customer PII have already left Hardcount
//     and the completion row is never written. audit_log would say nothing
//     happened, and an append-only compliance log gives no second chance to
//     write it.
//
// So the row records the attempt: an operator asked Hardcount for the
// tenant's customers, invoices, subscriptions, usage or audit log. That is
// the fact a chain of custody needs. It fails closed: if the row cannot be
// written, the handler returns 5xx and streams nothing. An export Hardcount
// cannot record is an export it does not perform.
//
// The row is written on its own transaction. An export is a read, so there
// is no business transaction to join (ADR-090's shared-fate LogInTx needs
// one). audit.Logger.Log detaches from the caller's cancellation
// (context.WithoutCancel). So a client that hangs up the moment the export
// starts still leaves the row behind.
//
// The row has no row count, on purpose. The count is unknown before the first
// page is fetched, and a number Hardcount cannot know to be true is a lie in
// a permanent record. resource_id is empty for the same reason: a bulk export
// has no single subject.
func (h *exportsHandler) auditExport(r *http.Request, tenantID, resourceType, filename string, filters map[string]any) error {
	return h.audit.Log(r.Context(), tenantID, domain.AuditActionExport, resourceType, "", "",
		map[string]any{
			"format":   "csv",
			"filename": filename,
			"filters":  filters,
		})
}

// exportScope describes what the export selected, for the audit row's
// metadata. An empty date filter is recorded as "all". "The operator took the
// entire table" is the fact worth having, and a missing key would read as a
// missing detail, not as an unfiltered dump.
func exportScope(from, to time.Time) map[string]any {
	if from.IsZero() && to.IsZero() {
		return map[string]any{"date_range": "all"}
	}
	m := map[string]any{}
	if !from.IsZero() {
		m["from"] = from.UTC().Format(time.RFC3339)
	}
	if !to.IsZero() {
		m["to"] = to.UTC().Format(time.RFC3339)
	}
	return m
}

// csvSafe neutralizes spreadsheet (CSV) formula injection. Excel, Google
// Sheets and LibreOffice read a cell as a formula when its first character is
// one of = + - @ TAB CR. So a customer-controlled value like "=HYPERLINK(...)"
// or "@SUM(...)" runs when the operator opens the export. A single quote in
// front of such a value makes it show as text. Empty strings pass through.
// It is applied to free-text columns that outsiders control (display names,
// external IDs, emails, codes, idempotency keys).
//
// The CSV is what an operator hands an auditor, and a file that runs code
// when opened is not evidence. The audit-log export makes this matter more,
// because customer display names reach it through resource_label.
func csvSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', 0x09, 0x0d:
		return "'" + s
	}
	return s
}

// flushAndContinue flushes the csv buffer to the underlying response
// so streaming clients see partial output. Without the flush, large
// exports buffer until completion.
func flushAndContinue(cw *csv.Writer, w http.ResponseWriter) error {
	cw.Flush()
	if err := cw.Error(); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// exportAbort makes a mid-stream failure visible in the file. By the time a
// store error or a ctx timeout hits, the HTTP status is already 200, because
// the headers went out with the first row. Without a marker, the stream would
// just end. The cut-off CSV would look like a complete export, and feed
// partial books into whatever the operator reconciles against.
//
// So exportAbort writes a trailing EXPORT_INCOMPLETE record, a signal a
// machine can check, and logs an error on the server. The documented
// contract: a row whose first cell is EXPORT_INCOMPLETE means discard the
// file and retry. The marker is best-effort by design: if the client
// connection itself died, the write fails silently, and that client is not
// reading anyway.
//
// auditExport runs before the stream, so the audit row is already written.
// An aborted export still leaves the true record: data was requested and
// partly handed over.
func exportAbort(ctx context.Context, cw *csv.Writer, resource string, err error) {
	slog.ErrorContext(ctx, "csv export aborted mid-stream — emitted EXPORT_INCOMPLETE marker",
		"resource", resource, "error", err)
	_ = cw.Write([]string{"EXPORT_INCOMPLETE", "the export aborted before completion — discard this file and retry"})
	cw.Flush()
}

// exportAuditFailed answers a request whose audit row could not be written.
// No CSV headers are set and no bytes are written yet, so this is a clean
// 500, and the export does not happen. audit.Logger.Log has already logged
// the cause and incremented hardcount_audit_write_errors_total.
func exportAuditFailed(w http.ResponseWriter, r *http.Request, resourceType string, err error) {
	slog.ErrorContext(r.Context(), "export REFUSED — its audit row could not be written (fail-closed)",
		"resource", resourceType, "error", err)
	respond.InternalError(w, r)
}

// timePtrCSV formats a *time.Time as RFC3339 or empty string. Used
// for nullable columns (canceled_at, paid_at, voided_at, etc).
func timePtrCSV(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// ---- Customers ----

func (h *exportsHandler) exportCustomers(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	if tenantID == "" {
		respond.Unauthorized(w, r, "missing tenant context")
		return
	}
	from, to, err := parseDateRange(r)
	if err != nil {
		respond.FromError(w, r, err, "export")
		return
	}

	filename := exportFilename("customers")
	if err := h.auditExport(r, tenantID, "customer", filename, exportScope(from, to)); err != nil {
		exportAuditFailed(w, r, "customer", err)
		return
	}

	cw := writeCSVHeaders(w, r, filename)
	defer cw.Flush()

	if err := cw.Write([]string{
		"id", "external_id", "display_name", "email", "status",
		"email_status", "created_at", "updated_at",
	}); err != nil {
		return
	}

	n := 0
	err = h.customers.StreamForExport(r.Context(), tenantID, from, to, func(c domain.Customer) error {
		if err := cw.Write([]string{
			c.ID, csvSafe(c.ExternalID), csvSafe(c.DisplayName), csvSafe(c.Email),
			string(c.Status),
			string(c.EmailStatus),
			c.CreatedAt.UTC().Format(time.RFC3339),
			c.UpdatedAt.UTC().Format(time.RFC3339),
		}); err != nil {
			return err
		}
		n++
		if n%exportPageSize == 0 {
			return flushAndContinue(cw, w)
		}
		return nil
	})
	if err != nil {
		exportAbort(r.Context(), cw, "customers", err)
		return
	}
	// Flush the last partial batch to the socket. The periodic flush above
	// runs only on an exportPageSize boundary. Without this call, an export
	// smaller than one batch would never reach http.Flusher. The deferred
	// cw.Flush() only pushes the csv.Writer into the ResponseWriter, not the
	// ResponseWriter onto the wire.
	_ = flushAndContinue(cw, w)
}

// ---- Invoices ----

func (h *exportsHandler) exportInvoices(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	if tenantID == "" {
		respond.Unauthorized(w, r, "missing tenant context")
		return
	}
	from, to, err := parseDateRange(r)
	if err != nil {
		respond.FromError(w, r, err, "export")
		return
	}

	filename := exportFilename("invoices")
	if err := h.auditExport(r, tenantID, "invoice", filename, exportScope(from, to)); err != nil {
		exportAuditFailed(w, r, "invoice", err)
		return
	}

	cw := writeCSVHeaders(w, r, filename)
	defer cw.Flush()

	if err := cw.Write([]string{
		"id", "invoice_number", "customer_id", "subscription_id",
		"status", "payment_status", "currency",
		"subtotal_cents", "tax_amount_cents", "discount_cents",
		"total_amount_cents", "amount_due_cents", "amount_paid_cents",
		"credits_applied_cents",
		"billing_period_start", "billing_period_end",
		// uncollectible_at sits beside its sibling dates. A written-off
		// invoice that is later recovered reaches status='paid'. Without
		// this column, its row is byte-identical to one never written off.
		// Recovering bad debt is a distinct accounting event, and the export
		// is the only lasting place an accountant can find one.
		//
		// tax_provider and tax_transaction_id are what the tenant needs to
		// claim bad-debt tax relief itself (ADR-111). Hardcount does not
		// reverse tax on a write-off, so the claim is the tenant's own, and it
		// has conditions. In the UK, the debt must have stayed unpaid for 6
		// months and be written off to a separate bad-debt account. In CA, NY
		// and TX, it must be charged off for federal income tax purposes.
		// Every other input those tests need is already here: status,
		// tax_amount_cents, uncollectible_at, due_at, paid_at and
		// amount_paid_cents.
		//
		// tax_reversed_at adds whether Hardcount already reversed the tax at
		// the provider, as it did for invoices written off before ADR-111.
		// That one fact decides whether the tenant's own claim would be a
		// double claim.
		"tax_provider", "tax_transaction_id", "tax_reversed_at",
		"issued_at", "due_at", "paid_at", "voided_at", "uncollectible_at",
		"created_at",
	}); err != nil {
		return
	}

	n := 0
	err = h.invoices.StreamForExport(r.Context(), tenantID, from, to, func(inv domain.Invoice) error {
		if err := cw.Write([]string{
			inv.ID, csvSafe(inv.InvoiceNumber), inv.CustomerID, inv.SubscriptionID,
			string(inv.Status), string(inv.PaymentStatus), inv.Currency,
			strconv.FormatInt(inv.SubtotalCents, 10),
			strconv.FormatInt(inv.TaxAmountCents, 10),
			strconv.FormatInt(inv.DiscountCents, 10),
			strconv.FormatInt(inv.TotalAmountCents, 10),
			strconv.FormatInt(inv.AmountDueCents, 10),
			strconv.FormatInt(inv.AmountPaidCents, 10),
			strconv.FormatInt(inv.CreditsAppliedCents, 10),
			inv.BillingPeriodStart.UTC().Format(time.RFC3339),
			inv.BillingPeriodEnd.UTC().Format(time.RFC3339),
			csvSafe(inv.TaxProvider),
			csvSafe(inv.TaxTransactionID),
			timePtrCSV(inv.TaxReversedAt),
			timePtrCSV(inv.IssuedAt),
			timePtrCSV(inv.DueAt),
			timePtrCSV(inv.PaidAt),
			timePtrCSV(inv.VoidedAt),
			timePtrCSV(inv.UncollectibleAt),
			inv.CreatedAt.UTC().Format(time.RFC3339),
		}); err != nil {
			return err
		}
		n++
		if n%exportPageSize == 0 {
			return flushAndContinue(cw, w)
		}
		return nil
	})
	if err != nil {
		exportAbort(r.Context(), cw, "invoices", err)
		return
	}
	// Flush the last partial batch to the socket. The periodic flush above
	// runs only on an exportPageSize boundary. Without this call, an export
	// smaller than one batch would never reach http.Flusher. The deferred
	// cw.Flush() only pushes the csv.Writer into the ResponseWriter, not the
	// ResponseWriter onto the wire.
	_ = flushAndContinue(cw, w)
}

// ---- Subscriptions ----

func (h *exportsHandler) exportSubscriptions(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	if tenantID == "" {
		respond.Unauthorized(w, r, "missing tenant context")
		return
	}
	from, to, err := parseDateRange(r)
	if err != nil {
		respond.FromError(w, r, err, "export")
		return
	}

	filename := exportFilename("subscriptions")
	if err := h.auditExport(r, tenantID, "subscription", filename, exportScope(from, to)); err != nil {
		exportAuditFailed(w, r, "subscription", err)
		return
	}

	cw := writeCSVHeaders(w, r, filename)
	defer cw.Flush()

	if err := cw.Write([]string{
		"id", "code", "display_name", "customer_id",
		"status", "billing_time",
		"trial_start_at", "trial_end_at",
		"started_at", "activated_at", "canceled_at",
		"current_billing_period_start", "current_billing_period_end",
		"next_billing_at",
		"plan_ids", // pipe-delimited list of plans on the sub
		"created_at",
	}); err != nil {
		return
	}

	// planIDs arrives pipe-joined from the query's aggregate. So the store
	// does not load every subscription's items just to join their plan ids
	// into one string.
	n := 0
	err = h.subscriptions.StreamForExport(r.Context(), tenantID, from, to, func(sub domain.Subscription, planIDs string) error {
		if err := cw.Write([]string{
			sub.ID, csvSafe(sub.Code), csvSafe(sub.DisplayName), sub.CustomerID,
			string(sub.Status), string(sub.BillingTime),
			timePtrCSV(sub.TrialStartAt), timePtrCSV(sub.TrialEndAt),
			timePtrCSV(sub.StartedAt), timePtrCSV(sub.ActivatedAt),
			timePtrCSV(sub.CanceledAt),
			timePtrCSV(sub.CurrentBillingPeriodStart),
			timePtrCSV(sub.CurrentBillingPeriodEnd),
			timePtrCSV(sub.NextBillingAt),
			planIDs,
			sub.CreatedAt.UTC().Format(time.RFC3339),
		}); err != nil {
			return err
		}
		n++
		if n%exportPageSize == 0 {
			return flushAndContinue(cw, w)
		}
		return nil
	})
	if err != nil {
		exportAbort(r.Context(), cw, "subscriptions", err)
		return
	}
	// Flush the last partial batch to the socket. The periodic flush above
	// runs only on an exportPageSize boundary. Without this call, an export
	// smaller than one batch would never reach http.Flusher. The deferred
	// cw.Flush() only pushes the csv.Writer into the ResponseWriter, not the
	// ResponseWriter onto the wire.
	_ = flushAndContinue(cw, w)
}

// ---- Usage events ----

func (h *exportsHandler) exportUsageEvents(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	if tenantID == "" {
		respond.Unauthorized(w, r, "missing tenant context")
		return
	}
	from, to, err := parseDateRange(r)
	if err != nil {
		respond.FromError(w, r, err, "export")
		return
	}
	// The range is required, because this table has no bound.
	if from.IsZero() || to.IsZero() {
		respond.BadRequest(w, r, "usage-events export requires `from` and `to` query params (RFC3339)")
		return
	}
	if to.Sub(from) > usageExportMaxSpanDays*24*time.Hour {
		respond.BadRequest(w, r, fmt.Sprintf("date range exceeds %d days — split into multiple calls", usageExportMaxSpanDays))
		return
	}
	// Optional filters, the same as GET /v1/usage-events, so the dashboard's
	// Export CSV downloads exactly the rows the Usage page lists. A malformed
	// dimensions pair is refused: dropping it would export unfiltered rows
	// as if filtered.
	q := r.URL.Query()
	dims, err := usage.ParseDimensionsParam(q.Get("dimensions"))
	if err != nil {
		respond.Validation(w, r, err.Error())
		return
	}
	exportFilter := usage.ExportFilter{CustomerID: q.Get("customer_id"), MeterID: q.Get("meter_id"), Dimensions: dims}

	filename := exportFilename("usage-events")
	scope := exportScope(from, to)
	if exportFilter.CustomerID != "" {
		scope["customer_id"] = exportFilter.CustomerID
	}
	if exportFilter.MeterID != "" {
		scope["meter_id"] = exportFilter.MeterID
	}
	if q.Get("dimensions") != "" {
		scope["dimensions"] = q.Get("dimensions")
	}
	if err := h.auditExport(r, tenantID, "usage_event", filename, scope); err != nil {
		exportAuditFailed(w, r, "usage_event", err)
		return
	}

	cw := writeCSVHeaders(w, r, filename)
	defer cw.Flush()

	// The provider_cost_* columns (ADR-079, in cents per ADR-130 §3) serve
	// the margin join in an operator's own data warehouse. That is the
	// checked peer workflow: Metronome exports events with cost metadata for
	// this purpose. The in-app margin card covers the common case, and the
	// CSV keeps finance reconciliation possible.
	if err := cw.Write([]string{
		"id", "customer_id", "meter_id",
		"quantity", "timestamp",
		"idempotency_key", "origin",
		"dimensions_json",
		"provider_cost_cents", "provider_cost_source",
	}); err != nil {
		return
	}

	n := 0
	err = h.usage.StreamForExportFiltered(r.Context(), tenantID, from, to, exportFilter, func(ev domain.UsageEvent) error {
		dimsJSON := ""
		if len(ev.Dimensions) > 0 {
			if b, err := json.Marshal(ev.Dimensions); err == nil {
				dimsJSON = string(b)
			}
		}
		costCents := ""
		if ev.ProviderCostMicros != nil {
			costCents = domain.MicrosToCents(*ev.ProviderCostMicros).String()
		}
		if err := cw.Write([]string{
			ev.ID, ev.CustomerID, ev.MeterID,
			ev.Quantity.String(),
			ev.Timestamp.UTC().Format(time.RFC3339),
			csvSafe(ev.IdempotencyKey),
			string(ev.Origin),
			dimsJSON,
			costCents, ev.ProviderCostSource,
		}); err != nil {
			return err
		}
		n++
		if n%exportPageSize == 0 {
			return flushAndContinue(cw, w)
		}
		return nil
	})
	if err != nil {
		exportAbort(r.Context(), cw, "usage_events", err)
		return
	}
	// Flush the last partial batch to the socket. The periodic flush above
	// runs only on an exportPageSize boundary. Without this call, an export
	// smaller than one batch would never reach http.Flusher. The deferred
	// cw.Flush() only pushes the csv.Writer into the ResponseWriter, not the
	// ResponseWriter onto the wire.
	_ = flushAndContinue(cw, w)
}

// ---- Audit log ----

// exportAuditLog streams the tenant's audit log as CSV: the compliance
// evidence pack, built on the server.
//
// audit.Logger.Stream applies no cap, so the export is bounded only by its
// filters. A cap would silently cut the evidence itself, and the operator
// would get a file that looks complete, with nothing in it saying otherwise.
//
// The export audits itself. The row is written before the stream begins, so
// it is always in the log. "Who took a copy of the audit log?" is the one
// question a tamper-evidence system must always be able to answer about
// itself.
//
// Whether the row is in the file is a different, weaker claim. An unfiltered
// export contains its own row: the snapshot is taken after the write, newest
// first, so the row is the first data row. But any filter that does not match
// the row excludes it, as it would exclude any other row. A clock-scoped
// export (?test_clock_id=) never contains it, because the export row is a
// wall-clock row with NULL sim columns. That is correct, not a gap, since the
// row is in the log either way. But in general the file does not certify
// itself.
func (h *exportsHandler) exportAuditLog(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	if tenantID == "" {
		respond.Unauthorized(w, r, "missing tenant context")
		return
	}
	// Use the param names and parsing of the audit-log read route
	// (audit.Handler.list), so the dashboard's on-screen filters and its
	// Export select the same rows.
	dateFrom, dateTo, err := timefilter.ParseRange(r, "date_from", "date_to")
	if err != nil {
		respond.FromError(w, r, err, "export")
		return
	}
	// The sim axis (ADR-090 §5), the simulated time on test-clock rows, is
	// part of the on-screen slice. An operator who looks at one simulation
	// and clicks Export must get that simulation, not the whole log. An
	// export that silently dropped the clock filter would hand an auditor a
	// file that looks like one clock's history but holds every tenant
	// action. That is the worst kind of wrong, because nothing in the file
	// says so.
	simFrom, simTo, err := timefilter.ParseRange(r, "sim_from", "sim_to")
	if err != nil {
		respond.FromError(w, r, err, "export")
		return
	}
	q := r.URL.Query()
	filter := audit.QueryFilter{
		ResourceType: q.Get("resource_type"),
		ResourceID:   q.Get("resource_id"),
		Action:       q.Get("action"),
		ActorID:      q.Get("actor_id"),
		DateFrom:     dateFrom,
		DateTo:       dateTo,
		TestClockID:  q.Get("test_clock_id"),
		SimFrom:      simFrom,
		SimTo:        simTo,
	}

	scope := exportScope(dateFrom, dateTo)
	for k, v := range map[string]string{
		"resource_type": filter.ResourceType,
		"resource_id":   filter.ResourceID,
		"action":        filter.Action,
		"actor_id":      filter.ActorID,
		// Recorded on the export's own audit row: "which slice was taken
		// out?" is the question this row exists to answer.
		"test_clock_id": filter.TestClockID,
	} {
		if v != "" {
			scope[k] = v
		}
	}
	if !simFrom.IsZero() {
		scope["sim_from"] = simFrom.UTC().Format(time.RFC3339)
	}
	if !simTo.IsZero() {
		scope["sim_to"] = simTo.UTC().Format(time.RFC3339)
	}

	filename := exportFilename("audit-log")
	if err := h.auditExport(r, tenantID, "audit_log", filename, scope); err != nil {
		exportAuditFailed(w, r, "audit_log", err)
		return
	}

	cw := writeCSVHeaders(w, r, filename)
	defer cw.Flush()

	// sim_effective_at and test_clock_id: a simulation's rows all share one
	// wall-clock created_at, because an advance settles everything at one
	// moment. So in a simulated tenant's CSV, created_at cannot tell the
	// reader when, in the simulation, anything happened. Only these two
	// columns can. After teardown (ADR-086), they are the only surviving
	// record that the clock existed at all. They are empty on wall-clock
	// rows.
	if err := cw.Write([]string{
		"id", "created_at", "actor_type", "actor_id", "actor_name",
		"action", "resource_type", "resource_id", "resource_label",
		"ip_address", "request_id", "sim_effective_at", "test_clock_id",
		"metadata_json",
	}); err != nil {
		return
	}

	n := 0
	streamErr := h.auditLog.Stream(r.Context(), tenantID, filter, func(e domain.AuditEntry) error {
		simAt := ""
		if e.SimEffectiveAt != nil {
			simAt = e.SimEffectiveAt.UTC().Format(time.RFC3339)
		}
		metaJSON := ""
		if len(e.Metadata) > 0 {
			if b, err := json.Marshal(e.Metadata); err == nil {
				metaJSON = string(b)
			}
		}
		// Run csvSafe on every column that is not an id Hardcount made or a
		// closed vocabulary. resource_label carries customer display names
		// and plan or subscription codes. actor_name is a customer's display
		// name on customer-actor rows. metadata is a free-form bag.
		//
		// request_id is neutralized too, on purpose. Hardcount makes it on
		// the server today (ADR-090 §6). Before that, chi's middleware
		// copied an inbound X-Request-Id as it came. audit_log is
		// append-only, so a row written before that change still carries
		// whatever string its caller chose, a live formula included. The
		// column cannot be cleaned afterwards. It can only be shown safely.
		//
		// ip_address gets the same treatment, for the same reason. An
		// exemption would rest on "TrustedRealIP only ever yields a
		// net.ParseIP-validated value". That is an argument about what the
		// code writes today, and the append-only rule defeats it: the log
		// outlives its writers. csvSafe on a well-formed IP changes nothing
		// (no address begins with = + - @). So an exemption would gain
		// nothing, and lose the one guarantee that has to hold for a
		// permanent record.
		if err := cw.Write([]string{
			e.ID,
			e.CreatedAt.UTC().Format(time.RFC3339),
			e.ActorType, e.ActorID, csvSafe(e.ActorName),
			e.Action, e.ResourceType, e.ResourceID, csvSafe(e.ResourceLabel),
			csvSafe(e.IPAddress), csvSafe(e.RequestID),
			simAt, e.TestClockID,
			csvSafe(metaJSON),
		}); err != nil {
			return err
		}
		n++
		if n%exportPageSize == 0 {
			return flushAndContinue(cw, w)
		}
		return nil
	})
	if streamErr != nil {
		exportAbort(r.Context(), cw, "audit_log", streamErr)
		return
	}
	_ = flushAndContinue(cw, w)
}
