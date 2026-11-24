package analytics

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"

	"github.com/gethardcount/hardcount/internal/domain"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/platform/logging"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// OverviewResponse aggregates the headline billing metrics for a tenant over
// a requested period, plus matching values for the prior period so the UI can
// render period-over-period deltas without a second round-trip.
type OverviewResponse struct {
	Period string `json:"period"`

	// Currency is the tenant's default currency. Revenue and OutstandingAR
	// are scoped to it: invoices in other currencies are excluded rather
	// than summed into a meaningless mixed-currency total. Each
	// ..._by_currency list names every currency instead.
	Currency string `json:"currency"`

	// Money (cents)
	Revenue       int64 `json:"revenue"`
	RevenuePrev   int64 `json:"revenue_prev"`
	OutstandingAR int64 `json:"outstanding_ar"`

	// OutstandingByCurrency is the amount still due on the same open invoices
	// OpenInvoices counts, one row per currency. OutstandingAR covers the
	// default currency only, so on its own it under-reports a tenant that
	// bills in more than one currency. Default currency first, then largest.
	OutstandingByCurrency []CurrencyAmount `json:"outstanding_by_currency"`

	// CreditBalanceByCurrency is the credit customers hold, one row per
	// currency (ADR-129): each ledger row stores its own currency, and
	// balances in two currencies are never added together. Default currency
	// first, then largest.
	CreditBalanceByCurrency []CurrencyAmount `json:"credit_balance_by_currency"`

	// CreditSoldByCurrency is the credit granted by the top-ups paid in the
	// period, per currency. A discounted top-up grants more than was paid,
	// so this is not part of Revenue. Ordered like CreditBalanceByCurrency.
	CreditSoldByCurrency []CurrencyAmount `json:"credit_sold_by_currency"`

	// UsageBilledByCurrency is the usage lines on the invoices issued in the
	// period, per currency, before tax. Ordered like CreditBalanceByCurrency.
	UsageBilledByCurrency []CurrencyAmount `json:"usage_billed_by_currency"`

	// Counts
	ActiveCustomers    int   `json:"active_customers"`
	NewCustomers       int   `json:"new_customers"`
	PaidInvoices       int   `json:"paid_invoices"`
	FailedPayments     int   `json:"failed_payments"`
	FailedPaymentsPrev int   `json:"failed_payments_prev"`
	OpenInvoices       int   `json:"open_invoices"`
	DunningActive      int   `json:"dunning_active"`
	RefundsAttention   int   `json:"refunds_needing_attention"`
	UsageEvents        int64 `json:"usage_events"`
	// CustomersOutOfCredit and CustomersLowOnCredit count customers with at
	// least one balance the credit balances list's balance=out or
	// balance=low filter picks. A customer with balances in two currencies
	// counts once.
	CustomersOutOfCredit int `json:"customers_out_of_credit"`
	CustomersLowOnCredit int `json:"customers_low_on_credit"`
}

// CurrencyAmount is an amount in one currency's minor units.
type CurrencyAmount struct {
	Currency    string `json:"currency"`
	AmountCents int64  `json:"amount_cents"`
}

func (h *Handler) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := auth.TenantID(ctx)
	period := parsePeriod(r.URL.Query().Get("period"))

	tx, err := h.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		slog.Log(ctx, logging.ErrorLevel(ctx, err), "analytics overview: begin tx", "error", err)
		respond.InternalError(w, r)
		return
	}
	defer func() { _ = tx.Rollback() }()

	resp := OverviewResponse{Period: period.Key}

	// Resolve the tenant's default currency once. Revenue and outstanding AR
	// are scoped to it, so the dashboard never sums amounts across
	// currencies into a corrupt total, and each by-currency list puts it
	// first. Defaults to USD when settings are unset (fresh tenant).
	defaultCurrency, err := defaultCurrencyFor(ctx, tx)
	if err != nil {
		slog.Log(ctx, logging.ErrorLevel(ctx, err), "analytics overview: default currency", "error", err)
		respond.InternalError(w, r)
		return
	}
	resp.Currency = defaultCurrency

	var err1, err2 error
	// Revenue: paid invoices inside the period vs. the prior period.
	resp.Revenue, err1 = sumPaidRevenue(ctx, tx, period.Start, period.End, defaultCurrency)
	resp.RevenuePrev, err2 = sumPaidRevenue(ctx, tx, period.PrevStart, period.PrevEnd, defaultCurrency)
	if err := firstErr(err1, err2); err != nil {
		slog.Log(ctx, logging.ErrorLevel(ctx, err), "analytics overview: revenue", "error", err)
		respond.InternalError(w, r)
		return
	}

	// Snapshot counts
	queries := []struct {
		name string
		dst  any
		sql  string
		args []any
	}{
		{"active_customers", &resp.ActiveCustomers,
			`SELECT COUNT(*) FROM customers WHERE status = 'active'`, nil},
		{"new_customers", &resp.NewCustomers,
			`SELECT COUNT(*) FROM customers WHERE created_at >= $1 AND created_at < $2`,
			[]any{period.Start, period.End}},
		{"outstanding_ar", &resp.OutstandingAR,
			`SELECT COALESCE(SUM(amount_due_cents), 0) FROM invoices WHERE status = 'finalized' AND payment_status != 'succeeded' AND currency = $1`,
			[]any{defaultCurrency}},
		{"paid_invoices", &resp.PaidInvoices,
			`SELECT COUNT(*) FROM invoices WHERE status = 'paid' AND currency = $1 AND paid_at >= $2 AND paid_at < $3`,
			[]any{defaultCurrency, period.Start, period.End}},
		// Open invoices only: a void or write-off keeps its last
		// payment_status, so it counted as a failed payment nobody was
		// collecting any more (sweep A7/E1, 2026-10-21).
		{"failed_payments", &resp.FailedPayments,
			`SELECT COUNT(*) FROM invoices WHERE status = 'finalized' AND payment_status = 'failed' AND created_at >= $1 AND created_at < $2`,
			[]any{period.Start, period.End}},
		// Same predicate over the previous window, so the trend compares like
		// with like (the dashboard used to compare against a hard-coded 0).
		{"failed_payments_prev", &resp.FailedPaymentsPrev,
			`SELECT COUNT(*) FROM invoices WHERE status = 'finalized' AND payment_status = 'failed' AND created_at >= $1 AND created_at < $2`,
			[]any{period.PrevStart, period.PrevEnd}},
		{"open_invoices", &resp.OpenInvoices,
			`SELECT COUNT(*) FROM invoices WHERE status = 'finalized' AND payment_status != 'succeeded'`, nil},
		{"dunning_active", &resp.DunningActive,
			// Terminal states are 'resolved' and 'escalated' — see
			// domain.DunningRunState. Pre-fix this read 'exhausted',
			// which is not a real state, so 'escalated' runs slipped
			// through and the dashboard reported every terminal-escalated
			// invoice as still actively retrying.
			`SELECT COUNT(*) FROM invoice_dunning_runs WHERE state NOT IN ('resolved', 'escalated')`, nil},
		// tenant_id is explicit even though the tx is RLS-scoped: the RLS
		// policy is an OR (bypass_rls OR tenant match), which Postgres can
		// only apply as a post-scan Filter. usage_events is the one table
		// here big enough for that to matter — without the predicate this
		// COUNT seq-scans every tenant's events on every dashboard load.
		{"usage_events", &resp.UsageEvents,
			`SELECT COUNT(*) FROM usage_events WHERE tenant_id = $1 AND timestamp >= $2 AND timestamp < $3`,
			[]any{tenantID, period.Start, period.End}},
		// Issued credit notes whose Stripe refund leg is failed, unknown or
		// stuck pending — i.e. a customer may be owed money that hasn't been
		// pushed back yet. The refund
		// is operator-retried (no auto-sweep), so surfacing the count is what
		// turns "an operator will notice" into "an operator is told".
		// The predicate is domain.CreditNoteRefundNeedsAttentionSQL, shared with the list
		// filter so the count and the list never disagree.
		{"refunds_needing_attention", &resp.RefundsAttention,
			`SELECT COUNT(*) FROM credit_notes WHERE ` + domain.CreditNoteRefundNeedsAttentionSQL, nil},
		// Customers with a balance the credit balances list's filter picks,
		// each counted once whatever its currencies. Both read the
		// predicate from domain.CreditBalanceFilterSQL.
		{"customers_out_of_credit", &resp.CustomersOutOfCredit,
			customersWithBalanceSQL(domain.CreditBalanceOut), []any{tenantID}},
		{"customers_low_on_credit", &resp.CustomersLowOnCredit,
			customersWithBalanceSQL(domain.CreditBalanceLow), []any{tenantID}},
	}
	for _, q := range queries {
		if err := tx.QueryRowContext(ctx, q.sql, q.args...).Scan(q.dst); err != nil {
			slog.Log(ctx, logging.ErrorLevel(ctx, err), "analytics overview", "query", q.name, "error", err)
			respond.InternalError(w, r)
			return
		}
	}

	resp.OutstandingByCurrency, err = outstandingByCurrency(ctx, tx, defaultCurrency)
	if err != nil {
		slog.Log(ctx, logging.ErrorLevel(ctx, err), "analytics overview: outstanding by currency", "error", err)
		respond.InternalError(w, r)
		return
	}

	resp.CreditBalanceByCurrency, err = creditBalanceByCurrency(ctx, tx, defaultCurrency)
	if err != nil {
		slog.Log(ctx, logging.ErrorLevel(ctx, err), "analytics overview: credit balance", "error", err)
		respond.InternalError(w, r)
		return
	}

	resp.CreditSoldByCurrency, err = creditSoldByCurrency(ctx, tx, period.Start, period.End, defaultCurrency)
	if err != nil {
		slog.Log(ctx, logging.ErrorLevel(ctx, err), "analytics overview: credit sold", "error", err)
		respond.InternalError(w, r)
		return
	}

	resp.UsageBilledByCurrency, err = usageBilledByCurrency(ctx, tx, period.Start, period.End, defaultCurrency)
	if err != nil {
		slog.Log(ctx, logging.ErrorLevel(ctx, err), "analytics overview: usage billed", "error", err)
		respond.InternalError(w, r)
		return
	}

	_ = tx.Commit()
	respond.JSON(w, r, http.StatusOK, resp)
}

// outstandingByCurrency sums amount_due over the open invoices (the
// open_invoices predicate) per currency, so the dashboard can name every
// currency it is owed in instead of showing the default-currency figure next to
// a count that includes other currencies. Never nil: an empty result encodes
// as [], not null.
func outstandingByCurrency(ctx context.Context, tx *sql.Tx, defaultCurrency string) ([]CurrencyAmount, error) {
	return currencyAmounts(ctx, tx, `
		SELECT currency, SUM(amount_due_cents)
		FROM invoices
		WHERE status = 'finalized' AND payment_status != 'succeeded' AND amount_due_cents > 0
		GROUP BY currency
		ORDER BY (currency = $1) DESC, SUM(amount_due_cents) DESC, currency
	`, defaultCurrency)
}

// creditBalanceByCurrency is the credit customers hold per currency:
// authoritative SUM(amount_cents) over the ledger, matching
// credit.GetBalance. Summing amount_cents is order-independent and exact: a
// late expiry entry can be inserted out of created_at order, so the latest
// row's balance_after is not the balance. Grouped by the currency each row stores (ADR-129); a currency
// whose balance is zero is left out. Ordered like outstandingByCurrency.
func creditBalanceByCurrency(ctx context.Context, tx *sql.Tx, defaultCurrency string) ([]CurrencyAmount, error) {
	return currencyAmounts(ctx, tx, `
		SELECT currency, COALESCE(SUM(amount_cents), 0)
		FROM customer_credit_ledger
		GROUP BY currency
		HAVING SUM(amount_cents) <> 0
		ORDER BY (currency = $1) DESC, SUM(amount_cents) DESC, currency
	`, defaultCurrency)
}

// defaultCurrencyFor returns the tenant's configured default currency, or
// "USD" when unset. Analytics money totals are scoped to this currency —
// invoices in other currencies are excluded rather than summed into a corrupt
// mixed-currency total. Returns a non-nil error only on a real DB failure (a
// missing settings row falls back to USD).
func defaultCurrencyFor(ctx context.Context, tx *sql.Tx) (string, error) {
	cur := "USD"
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(NULLIF(default_currency, ''), 'USD') FROM tenant_settings LIMIT 1`,
	).Scan(&cur); err != nil && err != sql.ErrNoRows {
		return "USD", err
	}
	return cur, nil
}

// sumPaidRevenue is what customers paid for the invoices paid in [start, end):
// the payment, by card or offline, which the paid flip sets from the amount
// due. Credit applied is not added: its money was counted when it was paid (a
// prepaid commit) or never came in (promotional credit, credit from a note), so
// adding it counted the same money twice. A credit note on an unpaid invoice
// lowers the amount due, so a waived part is not counted (ADR-061 amendment
// 2026-10-19). A note on an invoice already paid changes nothing: Revenue is
// before refunds.
func sumPaidRevenue(ctx context.Context, tx *sql.Tx, start, end any, currency string) (int64, error) {
	var v int64
	err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(i.amount_paid_cents), 0)
		FROM invoices i
		WHERE `+paidInPeriodSQL+` AND i.currency = $3
	`, start, end, currency).Scan(&v)
	return v, err
}

// paidInPeriodSQL is "paid in the period" over invoices aliased i, with the
// period's bounds as $1 and $2: status paid and paid_at in [$1, $2). Revenue
// and credit sold share it.
const paidInPeriodSQL = `i.status = 'paid' AND i.paid_at >= $1 AND i.paid_at < $2`

// issuedInPeriodSQL is "issued in the period" over invoices aliased i, with
// the period's bounds as $1 and $2: issued_at in [$1, $2), the same
// closed-open window paidInPeriodSQL uses, on an invoice still standing.
// issued_at is set when an invoice is issued (a draft has none); a void
// cancels the invoice, so it is left out.
const issuedInPeriodSQL = `i.status IN ('finalized', 'paid', 'uncollectible') AND i.issued_at >= $1 AND i.issued_at < $2`

// creditSoldByCurrency is the credit granted by the top-ups paid in [start,
// end): the commit line's commit_granted_cents, per currency, which is more
// than the payment when the top-up was sold at a discount. An unpaid top-up
// is a draft and has granted nothing. Ordered like outstandingByCurrency.
func creditSoldByCurrency(ctx context.Context, tx *sql.Tx, start, end any, defaultCurrency string) ([]CurrencyAmount, error) {
	return currencyAmounts(ctx, tx, `
		SELECT i.currency, SUM(li.commit_granted_cents)
		FROM invoices i
		JOIN invoice_line_items li ON li.invoice_id = i.id AND li.commit_granted_cents > 0
		WHERE i.billing_reason = 'topup' AND `+paidInPeriodSQL+`
		GROUP BY i.currency
		ORDER BY (i.currency = $3) DESC, SUM(li.commit_granted_cents) DESC, i.currency
	`, start, end, defaultCurrency)
}

// usageBilledByCurrency is the usage lines (line_type usage, before tax) on
// the invoices issued in [start, end), per currency. Ordered like
// outstandingByCurrency; a currency whose usage sums to zero is left out.
func usageBilledByCurrency(ctx context.Context, tx *sql.Tx, start, end any, defaultCurrency string) ([]CurrencyAmount, error) {
	return currencyAmounts(ctx, tx, `
		SELECT i.currency, SUM(li.amount_cents)
		FROM invoices i
		JOIN invoice_line_items li ON li.invoice_id = i.id AND li.line_type = 'usage'
		WHERE `+issuedInPeriodSQL+`
		GROUP BY i.currency
		HAVING SUM(li.amount_cents) <> 0
		ORDER BY (i.currency = $3) DESC, SUM(li.amount_cents) DESC, i.currency
	`, start, end, defaultCurrency)
}

// customersWithBalanceSQL counts the customers with at least one balance
// filter picks, over the grouping the credit balances list uses
// (credit.PostgresStore.ListBalances). $1 is the tenant id: explicit, as in
// that list, so the ledger scan uses its tenant index.
func customersWithBalanceSQL(filter domain.CreditBalanceFilter) string {
	return `SELECT COUNT(DISTINCT customer_id) FROM (
		SELECT customer_id FROM customer_credit_ledger WHERE tenant_id = $1
		GROUP BY customer_id, currency
		HAVING ` + domain.CreditBalanceFilterSQL(filter) + `
	) balances`
}

// currencyAmounts runs a query whose rows are (currency, amount). Never nil:
// an empty result encodes as [], not null.
func currencyAmounts(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]CurrencyAmount, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []CurrencyAmount{}
	for rows.Next() {
		var a CurrencyAmount
		if err := rows.Scan(&a.Currency, &a.AmountCents); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
