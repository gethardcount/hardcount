package invoice

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tax"
)

type memStore struct {
	invoices  map[string]domain.Invoice
	lineItems map[string][]domain.InvoiceLineItem
	// pausedSubs: subscriptions with pause_collection in force
	// (CollectionPausedTx); pauseErr fails that read.
	pausedSubs map[string]bool
	pauseErr   error
}

func newMemStore() *memStore {
	return &memStore{
		invoices:  make(map[string]domain.Invoice),
		lineItems: make(map[string][]domain.InvoiceLineItem),
	}
}

func (m *memStore) Create(_ context.Context, tenantID string, inv domain.Invoice) (domain.Invoice, error) {
	inv.ID = fmt.Sprintf("hc_inv_%d", len(m.invoices)+1)
	inv.TenantID = tenantID
	now := time.Now().UTC()
	inv.CreatedAt = now
	inv.UpdatedAt = now
	inv.AutoChargePending = collectionQueuedAtFinalize(inv.Status, inv.PaymentStatus)
	m.invoices[inv.ID] = inv
	return inv, nil
}

func (m *memStore) Get(_ context.Context, tenantID, id string) (domain.Invoice, error) {
	inv, ok := m.invoices[id]
	if !ok || inv.TenantID != tenantID {
		return domain.Invoice{}, errs.ErrNotFound
	}
	return inv, nil
}

func (m *memStore) List(_ context.Context, filter ListFilter) ([]domain.Invoice, int, error) {
	var result []domain.Invoice
	for _, inv := range m.invoices {
		if inv.TenantID != filter.TenantID {
			continue
		}
		if filter.Status != "" && string(inv.Status) != filter.Status {
			continue
		}
		result = append(result, inv)
	}
	return result, len(result), nil
}

func (m *memStore) SetPublicToken(_ context.Context, tenantID, invoiceID, token string) error {
	inv, ok := m.invoices[invoiceID]
	if !ok || inv.TenantID != tenantID {
		return errs.ErrNotFound
	}
	if inv.Status == domain.InvoiceDraft {
		return errs.ErrNotFound
	}
	inv.PublicToken = token
	inv.UpdatedAt = time.Now().UTC()
	m.invoices[invoiceID] = inv
	return nil
}

func (m *memStore) GetByPublicToken(_ context.Context, token string) (domain.Invoice, bool, error) {
	if token == "" {
		return domain.Invoice{}, false, errs.ErrNotFound
	}
	for _, inv := range m.invoices {
		if inv.PublicToken == token {
			return inv, false, nil
		}
	}
	return domain.Invoice{}, false, errs.ErrNotFound
}

func (m *memStore) ListChargeAttemptsByInvoice(_ context.Context, _, _ string) ([]domain.InvoiceChargeAttempt, error) {
	return nil, nil
}

func (m *memStore) UpdateStatus(_ context.Context, tenantID, id string, status domain.InvoiceStatus) (domain.Invoice, error) {
	inv, ok := m.invoices[id]
	if !ok || inv.TenantID != tenantID {
		return domain.Invoice{}, errs.ErrNotFound
	}
	inv.Status = status
	inv.UpdatedAt = time.Now().UTC()
	// Mirrors updateStatusInTx: the queue flag moves with the status.
	switch status {
	case domain.InvoiceFinalized:
		if inv.PaymentStatus == domain.PaymentPending {
			inv.AutoChargePending = true
		}
	case domain.InvoiceVoided, domain.InvoiceUncollectible:
		inv.AutoChargePending = false
	}
	m.invoices[id] = inv
	return inv, nil
}

// Prior variant: the fake's "locked" row is the current in-memory invoice —
// exact, since the fake is single-threaded.
func (m *memStore) UpdateStatusWithReversalPrior(ctx context.Context, tenantID, id string, status domain.InvoiceStatus, fn func(tx *sql.Tx, locked domain.Invoice) error) (domain.Invoice, error) {
	return m.UpdateStatusWithReversalPriorTx(ctx, nil, tenantID, id, status, fn)
}

// ChargeHeldTx: the fake does not model the charge lease (its claims ignore
// it too), so no charge ever holds an invoice here. The hold is pinned
// against real Postgres in charge_hold_guard_integration_test.go.
func (m *memStore) ChargeHeldTx(_ context.Context, _ *sql.Tx, _ string) (domain.ChargeHoldState, domain.ChargeHold, error) {
	return domain.ChargeHoldNone, domain.ChargeHold{}, nil
}

func (m *memStore) ChargeHoldStateOf(_ context.Context, _, _ string) (domain.ChargeHoldState, error) {
	return domain.ChargeHoldNone, nil
}

// MarkPaidOffline: check sees the current in-memory invoice as the locked row.
// Like the real store, the paid flip sets paid_out_of_band and no PaymentIntent.
func (m *memStore) MarkPaidOffline(ctx context.Context, tenantID, id string, paidAt time.Time, check func(tx *sql.Tx, locked domain.Invoice) error) (domain.Invoice, error) {
	if inv, ok := m.invoices[id]; ok && inv.TenantID == tenantID && check != nil {
		if err := check(nil, inv); err != nil {
			return domain.Invoice{}, err
		}
	}
	wasPaid := m.invoices[id].Status == domain.InvoicePaid
	inv, err := m.MarkPaid(ctx, tenantID, id, "", paidAt)
	if err != nil || wasPaid {
		return inv, err
	}
	inv.PaidOutOfBand = true
	m.invoices[id] = inv
	return inv, nil
}

// SettleZeroDue mirrors the real store's predicate. The fake has no charge
// hold, so it never refuses for one.
func (m *memStore) SettleZeroDue(ctx context.Context, tenantID, id string, paidAt time.Time) (domain.Invoice, bool, error) {
	inv, ok := m.invoices[id]
	if !ok || inv.TenantID != tenantID {
		return domain.Invoice{}, false, errs.ErrNotFound
	}
	if inv.Status != domain.InvoiceFinalized || inv.AmountDueCents != 0 ||
		(inv.TaxStatus != "" && inv.TaxStatus != domain.InvoiceTaxOK) ||
		(inv.PaymentStatus != domain.PaymentPending && inv.PaymentStatus != domain.PaymentFailed) {
		return inv, false, nil
	}
	paid, err := m.MarkPaid(ctx, tenantID, id, "", paidAt)
	return paid, err == nil, err
}

// Tx variant: the fake ignores tx; the closure sees the current in-memory
// invoice as the locked prior, and an error restores it (the rollback).
func (m *memStore) UpdateStatusWithReversalPriorTx(ctx context.Context, _ *sql.Tx, tenantID, id string, status domain.InvoiceStatus, fn func(tx *sql.Tx, prior domain.Invoice) error) (domain.Invoice, error) {
	prev, ok := m.invoices[id]
	if !ok || prev.TenantID != tenantID {
		return domain.Invoice{}, errs.ErrNotFound
	}
	inv, err := m.UpdateStatus(ctx, tenantID, id, status)
	if err != nil {
		return domain.Invoice{}, err
	}
	if fn != nil {
		if err := fn(nil, prev); err != nil {
			m.invoices[id] = prev
			return domain.Invoice{}, err
		}
	}
	return inv, nil
}

func (m *memStore) FinalizeWithDates(_ context.Context, tenantID, id string, issuedAt, dueAt time.Time, taxedLineIDs []string, onFinalized func(tx *sql.Tx, inv domain.Invoice) error) (domain.Invoice, error) {
	inv, ok := m.invoices[id]
	if !ok || inv.TenantID != tenantID {
		return domain.Invoice{}, errs.ErrNotFound
	}
	// As Postgres: the lines must be the ones read before tax (#1298).
	if inv.Status == domain.InvoiceDraft {
		have := m.lineItems[id]
		same := len(have) == len(taxedLineIDs)
		for i := 0; same && i < len(have); i++ {
			same = slices.Contains(taxedLineIDs, have[i].ID)
		}
		if !same {
			return domain.Invoice{}, errLinesChanged()
		}
	}
	inv.Status = domain.InvoiceFinalized
	inv.AutoChargePending = inv.PaymentStatus == domain.PaymentPending
	inv.IssuedAt = &issuedAt
	inv.DueAt = &dueAt
	inv.UpdatedAt = time.Now().UTC()
	m.invoices[id] = inv
	if onFinalized != nil {
		if err := onFinalized(nil, inv); err != nil {
			return domain.Invoice{}, err
		}
	}
	return m.mintToken(id), nil
}

// CollectionPausedTx reads pausedSubs; the tx is ignored (nil here).
func (m *memStore) CollectionPausedTx(_ context.Context, _ *sql.Tx, invoiceID string, _ time.Time) (bool, error) {
	if m.pauseErr != nil {
		return false, m.pauseErr
	}
	inv, ok := m.invoices[invoiceID]
	if !ok {
		return false, errs.ErrNotFound
	}
	return m.pausedSubs[inv.SubscriptionID], nil
}

// FinalizeDraftTx is FinalizeDraft; the memStore has no transaction.
func (m *memStore) FinalizeDraftTx(ctx context.Context, _ *sql.Tx, tenantID, id string, onFinalized func(tx *sql.Tx, inv domain.Invoice) error) (domain.Invoice, error) {
	return m.FinalizeDraft(ctx, tenantID, id, onFinalized)
}

func (m *memStore) FinalizeDraft(ctx context.Context, tenantID, id string, onFinalized func(tx *sql.Tx, inv domain.Invoice) error) (domain.Invoice, error) {
	inv, err := m.UpdateStatus(ctx, tenantID, id, domain.InvoiceFinalized)
	if err != nil {
		return inv, err
	}
	if onFinalized != nil {
		if err := onFinalized(nil, inv); err != nil {
			return domain.Invoice{}, err
		}
	}
	return m.mintToken(id), nil
}

// mintToken gives a finalized invoice its hosted-page token when it has
// none and returns the invoice, as the Postgres store's finalize does in its
// transaction (finalizedTx).
func (m *memStore) mintToken(id string) domain.Invoice {
	inv := m.invoices[id]
	if inv.PublicToken == "" {
		if token, err := GeneratePublicToken(); err == nil {
			inv.PublicToken = token
			m.invoices[id] = inv
		}
	}
	return inv
}

func (m *memStore) MarkPaid(_ context.Context, tenantID, id, stripeID string, paidAt time.Time) (domain.Invoice, error) {
	inv, ok := m.invoices[id]
	if !ok || inv.TenantID != tenantID {
		return domain.Invoice{}, errs.ErrNotFound
	}
	// Mirror PostgresStore.MarkPaid's state-machine guard (added
	// 2026-05-22): reject draft (finalize first) and voided
	// (terminal); reject tax_status != ok (authoritative amounts).
	switch inv.Status {
	case domain.InvoiceFinalized, domain.InvoiceUncollectible, domain.InvoicePaid:
		// ok
	default:
		return domain.Invoice{}, errs.InvalidState("cannot mark invoice paid from status " + string(inv.Status))
	}
	if inv.TaxStatus != "" && inv.TaxStatus != domain.InvoiceTaxOK {
		return domain.Invoice{}, errs.InvalidState("cannot mark invoice paid with tax_status=" + string(inv.TaxStatus))
	}
	inv.AutoChargePending = false // mirrors markPaidReportingTransition
	inv.Status = domain.InvoicePaid
	inv.PaymentStatus = domain.PaymentSucceeded
	inv.StripePaymentIntentID = stripeID
	inv.AmountPaidCents = inv.AmountDueCents
	inv.AmountDueCents = 0
	inv.PaidAt = &paidAt
	m.invoices[id] = inv
	return inv, nil
}

func (m *memStore) ListLineItems(_ context.Context, _, invoiceID string) ([]domain.InvoiceLineItem, error) {
	return m.lineItems[invoiceID], nil
}

// TestRetryProviderConfigErrors_FlushesStuckRows covers ADR-019.
// After a successful Stripe re-connect, the service should flush
// every invoice in the (tenant, livemode) partition that was
// stuck on provider_not_configured / provider_auth — and skip
// rows in unrelated states.
func TestRetryProviderConfigErrors_FlushesStuckRows(t *testing.T) {
	store := newMemStore()
	now := time.Now().UTC()

	// Three invoices: stuck-provider-not-configured, stuck-provider-auth,
	// and tax-OK (already healthy — should be skipped).
	store.invoices["inv_stuck_a"] = domain.Invoice{
		ID: "inv_stuck_a", TenantID: "t1", CustomerID: "cus_a",
		Status: domain.InvoiceDraft,
		TaxFacts: domain.TaxFacts{
			TaxStatus: domain.InvoiceTaxPending, TaxErrorCode: "provider_not_configured",
		},
		BillingReason: "subscription_cycle",
		Currency:      "USD", CreatedAt: now,
	}
	store.invoices["inv_stuck_b"] = domain.Invoice{
		ID: "inv_stuck_b", TenantID: "t1", CustomerID: "cus_b",
		Status: domain.InvoiceDraft,
		TaxFacts: domain.TaxFacts{
			TaxStatus: domain.InvoiceTaxPending, TaxErrorCode: "provider_auth",
		},
		BillingReason: "subscription_cycle",
		Currency:      "USD", CreatedAt: now,
	}
	store.invoices["inv_healthy"] = domain.Invoice{
		ID: "inv_healthy", TenantID: "t1", CustomerID: "cus_c",
		Status: domain.InvoiceDraft, TaxFacts: domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK},
		BillingReason: "subscription_cycle", Currency: "USD", CreatedAt: now,
	}

	// Stub TaxRetrier that just flips status to ok — equivalent to
	// the engine succeeding after fresh creds land.
	stub := &stubTaxRetrier{store: store}
	svc := NewService(store, nil, nil)
	svc.SetTaxRetrier(stub)

	processed, errs := svc.RetryProviderConfigErrors(context.Background(), "t1", false)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if processed != 2 {
		t.Errorf("processed: got %d, want 2 (the two stuck rows)", processed)
	}
	if stub.calls != 2 {
		t.Errorf("RetryTaxForInvoice calls: got %d, want 2", stub.calls)
	}
	// Healthy invoice was never iterated.
	for _, id := range stub.calledOn {
		if id == "inv_healthy" {
			t.Errorf("retry should not touch healthy invoice")
		}
	}
}

// stubTaxRetrier mimics a successful engine retry: flips
// tax_status to ok on the row.
type stubTaxRetrier struct {
	store    *memStore
	calls    int
	calledOn []string
}

// RetryTaxForInvoice resolves the tax and runs then as the store would, on
// the write's "transaction": a then error puts the row back as it was and is
// returned.
func (s *stubTaxRetrier) RetryTaxForInvoice(ctx context.Context, tenantID, invoiceID string,
	then func(ctx context.Context, tx *sql.Tx, taxed domain.Invoice) error,
) (domain.Invoice, error) {
	s.calls++
	s.calledOn = append(s.calledOn, invoiceID)
	inv, ok := s.store.invoices[invoiceID]
	if !ok || inv.TenantID != tenantID {
		return domain.Invoice{}, errs.ErrNotFound
	}
	before := inv
	inv.TaxStatus = domain.InvoiceTaxOK
	inv.TaxPendingReason = ""
	inv.TaxErrorCode = ""
	s.store.invoices[invoiceID] = inv
	if then != nil {
		if err := then(ctx, nil, inv); err != nil {
			s.store.invoices[invoiceID] = before
			return domain.Invoice{}, err
		}
	}
	return s.store.invoices[invoiceID], nil
}

// ComputeTaxForInvoice mirrors the finalize-time compute: resolve tax to ok
// on a draft regardless of incoming tax_status. The stub leaves the amount
// untouched (the real engine runs ApplyTaxToLineItems); tests that assert on
// computed tax amounts use the real engine via integration coverage.
func (s *stubTaxRetrier) ComputeTaxForInvoice(_ context.Context, tenantID, invoiceID string) (domain.Invoice, error) {
	inv, ok := s.store.invoices[invoiceID]
	if !ok || inv.TenantID != tenantID {
		return domain.Invoice{}, errs.ErrNotFound
	}
	inv.TaxStatus = domain.InvoiceTaxOK
	s.store.invoices[invoiceID] = inv
	return inv, nil
}

// ListCustomerDataInvalidErrors — per-customer flush stub mirroring
// ListProviderConfigErrors. Filters on customer_id + status=draft +
// tax_status pending|failed + tax_error_code=customer_data_invalid.
func (m *memStore) ListCustomerDataInvalidErrors(_ context.Context, tenantID, customerID string) ([]domain.Invoice, error) {
	var out []domain.Invoice
	for _, inv := range m.invoices {
		if inv.TenantID != tenantID || inv.CustomerID != customerID {
			continue
		}
		if inv.Status != domain.InvoiceDraft {
			continue
		}
		if inv.TaxStatus != domain.InvoiceTaxPending {
			continue
		}
		if inv.TaxErrorCode != "customer_data_invalid" {
			continue
		}
		out = append(out, inv)
	}
	return out, nil
}

func (m *memStore) ListProviderConfigErrors(_ context.Context, tenantID string, _ bool) ([]domain.Invoice, error) {
	// memStore doesn't track livemode (mock fixtures are single-mode);
	// the real PostgresStore impl filters by livemode in the WHERE.
	var out []domain.Invoice
	for _, inv := range m.invoices {
		if inv.TenantID != tenantID {
			continue
		}
		if inv.Status != domain.InvoiceDraft {
			continue
		}
		if inv.TaxStatus != domain.InvoiceTaxPending {
			continue
		}
		if inv.TaxErrorCode != "provider_not_configured" && inv.TaxErrorCode != "provider_auth" {
			continue
		}
		out = append(out, inv)
	}
	return out, nil
}

// ListPendingTaxRetryForClock — ADR-029 Phase 2 stub for the in-memory
// test store. The narrow service tests in this file don't exercise the
// catchup path; integration tests cover it. Returning nothing here
// satisfies the interface without faking sub→clock joins in memory.
func (m *memStore) ListPendingTaxRetryForClock(_ context.Context, _, _ string, _ []string, _, _ int) ([]domain.Invoice, error) {
	return nil, nil
}

func (m *memStore) ListPendingTaxRetry(_ context.Context, batch int, retryableCodes []string, maxAttempts int, _ bool) ([]domain.Invoice, error) {
	if batch <= 0 {
		batch = 50
	}
	allowed := make(map[string]struct{}, len(retryableCodes))
	for _, c := range retryableCodes {
		allowed[c] = struct{}{}
	}
	now := time.Now().UTC()
	var out []domain.Invoice
	for _, inv := range m.invoices {
		if inv.Status != domain.InvoiceDraft {
			continue
		}
		if inv.TaxStatus != domain.InvoiceTaxPending {
			continue
		}
		if _, ok := allowed[inv.TaxErrorCode]; !ok {
			continue
		}
		if inv.TaxRetryCount >= maxAttempts {
			continue
		}
		if inv.TaxNextRetryAt != nil && inv.TaxNextRetryAt.After(now) {
			continue
		}
		out = append(out, inv)
		if len(out) >= batch {
			break
		}
	}
	return out, nil
}

// DeferTaxWork mirrors the real store: the pending lists skip the invoice
// until next.
func (m *memStore) DeferTaxWork(_ context.Context, _, id string, next time.Time) error {
	inv, ok := m.invoices[id]
	if !ok {
		return errs.ErrNotFound
	}
	inv.TaxNextRetryAt = &next
	m.invoices[id] = inv
	return nil
}

// taxWorkDeferred reports an invoice a failed commit or reversal has put off.
func taxWorkDeferred(inv domain.Invoice) bool {
	return inv.TaxNextRetryAt != nil && inv.TaxNextRetryAt.After(time.Now())
}

func (m *memStore) ListPendingTaxCommit(_ context.Context, batch int, _ bool) ([]domain.Invoice, error) {
	if batch <= 0 {
		batch = 50
	}
	var out []domain.Invoice
	for _, inv := range m.invoices {
		if inv.Status != domain.InvoiceFinalized || inv.TaxStatus != domain.InvoiceTaxOK || taxWorkDeferred(inv) {
			continue
		}
		if inv.TaxProvider != "stripe_tax" || inv.TaxCalculationID == "" || inv.TaxTransactionID != "" {
			continue
		}
		out = append(out, inv)
		if len(out) >= batch {
			break
		}
	}
	return out, nil
}

func (m *memStore) ListPendingTaxReversal(_ context.Context, batch int, _ bool) ([]domain.Invoice, error) {
	if batch <= 0 {
		batch = 50
	}
	var out []domain.Invoice
	for _, inv := range m.invoices {
		if inv.Status != domain.InvoiceVoided && inv.Status != domain.InvoiceUncollectible {
			continue
		}
		if inv.TaxProvider != "stripe_tax" || inv.TaxTransactionID == "" || inv.TaxReversedAt != nil {
			continue
		}
		if inv.IsSimulated || taxWorkDeferred(inv) {
			continue
		}
		out = append(out, inv)
		if len(out) >= batch {
			break
		}
	}
	return out, nil
}

func (m *memStore) MarkTaxReversed(_ context.Context, tenantID, id string) error {
	// Match the real store: the UPDATE is RLS-scoped + guarded by
	// `tax_reversed_at IS NULL`, so a missing/wrong-tenant id or an
	// already-stamped row is a silent no-op (zero rows), never an error.
	inv, ok := m.invoices[id]
	if !ok || inv.TenantID != tenantID || inv.TaxReversedAt != nil {
		return nil
	}
	now := time.Now().UTC()
	inv.TaxReversedAt = &now
	m.invoices[id] = inv
	return nil
}

func (m *memStore) CreateWithLineItems(_ context.Context, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem) (domain.Invoice, error) {
	// Emulate the proration dedup partial unique index. Without this, tests
	// that exercise retry-after-partial-failure paths silently double-insert
	// rows the real DB would have rejected.
	if inv.SourcePlanChangedAt != nil {
		for _, existing := range m.invoices {
			if existing.TenantID == tenantID && existing.SubscriptionID == inv.SubscriptionID &&
				existing.SourcePlanChangedAt != nil && existing.SourcePlanChangedAt.Equal(*inv.SourcePlanChangedAt) {
				return domain.Invoice{}, errs.ErrAlreadyExists
			}
		}
	}

	inv.ID = fmt.Sprintf("hc_inv_%d", len(m.invoices)+1)
	inv.TenantID = tenantID
	now := time.Now().UTC()
	inv.CreatedAt = now
	inv.UpdatedAt = now
	inv.AutoChargePending = collectionQueuedAtFinalize(inv.Status, inv.PaymentStatus)
	m.invoices[inv.ID] = inv
	for _, item := range items {
		item.InvoiceID = inv.ID
		item.TenantID = tenantID
		m.lineItems[inv.ID] = append(m.lineItems[inv.ID], item)
	}
	return inv, nil
}

// CreateNumberedAudited mirrors the store contract (ADR-090): number runs
// first and names the invoice, emit runs after the rows exist and before
// "commit"; an emit error rolls the write back and surfaces. The fake has no
// tx, so both closures get a nil *sql.Tx — enough to prove the service passes
// them and propagates their errors.
func (m *memStore) CreateNumberedAudited(ctx context.Context, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem, number func(tx *sql.Tx) (string, error), emit func(tx *sql.Tx, out domain.Invoice) error) (domain.Invoice, error) {
	if number != nil {
		num, err := number(nil)
		if err != nil {
			return domain.Invoice{}, fmt.Errorf("allocate invoice number: %w", err)
		}
		inv.InvoiceNumber = num
	}
	out, err := m.CreateWithLineItems(ctx, tenantID, inv, items)
	if err != nil {
		return domain.Invoice{}, err
	}
	if emit != nil {
		if err := emit(nil, out); err != nil {
			delete(m.invoices, out.ID) // shared fate: roll the write back
			delete(m.lineItems, out.ID)
			return domain.Invoice{}, fmt.Errorf("audit emission: %w", err)
		}
	}
	return out, nil
}

func (m *memStore) AddLineItemAtomicAudited(_ context.Context, tenantID, invoiceID string, item domain.InvoiceLineItem, emit func(tx *sql.Tx, item domain.InvoiceLineItem, inv domain.Invoice) error) (domain.InvoiceLineItem, domain.Invoice, error) {
	inv, ok := m.invoices[invoiceID]
	if !ok || inv.TenantID != tenantID {
		return domain.InvoiceLineItem{}, domain.Invoice{}, errs.ErrNotFound
	}
	if inv.Status != domain.InvoiceDraft {
		return domain.InvoiceLineItem{}, domain.Invoice{},
			fmt.Errorf("can only add line items to draft invoices, current status: %s", inv.Status)
	}
	before := inv
	beforeItems := len(m.lineItems[invoiceID])

	item.InvoiceID = invoiceID
	item.TenantID = tenantID
	item.Currency = inv.Currency
	item.ID = fmt.Sprintf("hc_ili_%d", len(m.lineItems[invoiceID])+1)
	m.lineItems[invoiceID] = append(m.lineItems[invoiceID], item)

	var subtotal int64
	for _, li := range m.lineItems[invoiceID] {
		subtotal += li.AmountCents
	}
	total := subtotal + inv.TaxAmountCents - inv.DiscountCents
	amountDue := total - inv.AmountPaidCents - inv.CreditsAppliedCents
	if amountDue < 0 {
		amountDue = 0
	}
	inv.SubtotalCents = subtotal
	inv.TotalAmountCents = total
	inv.AmountDueCents = amountDue
	inv.UpdatedAt = time.Now().UTC()
	m.invoices[invoiceID] = inv

	if emit != nil {
		if err := emit(nil, item, inv); err != nil {
			// shared fate: undo the line insert AND the totals rewrite
			m.lineItems[invoiceID] = m.lineItems[invoiceID][:beforeItems]
			m.invoices[invoiceID] = before
			return domain.InvoiceLineItem{}, domain.Invoice{}, fmt.Errorf("audit emission: %w", err)
		}
	}
	return item, inv, nil
}

// memHold is the token memStore's claims hand out: the fake does not model
// the hold itself, only whether the claim's predicate admitted the invoice.
var memHold = domain.ChargeHold{Until: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)}

func (m *memStore) ClaimChargeForManualCollect(_ context.Context, _, id string) (domain.ChargeHold, error) {
	inv, ok := m.invoices[id]
	if !ok {
		return domain.ChargeHold{}, errs.ErrNotFound
	}
	// Mirrors the manual-collect claim's predicate (sans the hold, which
	// memStore doesn't model): finalized, pending/failed, owing.
	if (inv.PaymentStatus == domain.PaymentPending || inv.PaymentStatus == domain.PaymentFailed) &&
		inv.Status == domain.InvoiceFinalized && inv.AmountDueCents > 0 {
		return memHold, nil
	}
	return domain.ChargeHold{}, nil
}

func (m *memStore) ReleaseChargeHold(_ context.Context, _, _ string, _ domain.ChargeHold) error {
	return nil
}

// memNumberer is a deterministic in-memory InvoiceNumberer for tests.
// Hands out HC-000001, HC-000002, ... so assertions on invoice numbers
// don't depend on clock or DB state.
type memNumberer struct {
	next int
}

func newMemNumberer() *memNumberer { return &memNumberer{} }

// NextInvoiceNumberTx ignores the tx: the fake keeps its count in memory.
func (m *memNumberer) NextInvoiceNumberTx(_ context.Context, _ *sql.Tx, _ string) (string, error) {
	m.next++
	return fmt.Sprintf("HC-%06d", m.next), nil
}

func TestCreate(t *testing.T) {
	svc := NewService(newMemStore(), nil, newMemNumberer())
	ctx := context.Background()

	t.Run("valid", func(t *testing.T) {
		inv, err := svc.Create(ctx, "t1", CreateInput{
			CustomerID:         "cus_1",
			SubscriptionID:     "sub_1",
			BillingPeriodStart: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
			BillingPeriodEnd:   time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if inv.Status != domain.InvoiceDraft {
			t.Errorf("got status %q, want draft", inv.Status)
		}
		if inv.PaymentStatus != domain.PaymentPending {
			t.Errorf("got payment_status %q, want pending", inv.PaymentStatus)
		}
		// Canonical uppercase ISO-4217 (matches the tenant default "USD" that
		// analytics/dunning revenue queries filter on); empty-input default.
		if inv.Currency != "USD" {
			t.Errorf("got currency %q, want USD", inv.Currency)
		}
		if inv.NetPaymentTermDays != 30 {
			t.Errorf("got net_payment_term %d, want 30", inv.NetPaymentTermDays)
		}
		if inv.InvoiceNumber == "" {
			t.Error("invoice_number should be generated")
		}
		if inv.IssuedAt == nil {
			t.Error("issued_at should be set")
		}
		if inv.DueAt == nil {
			t.Error("due_at should be set")
		}
	})

	t.Run("missing customer_id", func(t *testing.T) {
		_, err := svc.Create(ctx, "t1", CreateInput{SubscriptionID: "s"})
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("one-off invoice — subscription_id optional", func(t *testing.T) {
		// Operator-issued one-off invoices (composer on the customer page)
		// have no parent subscription. Create must succeed, persist an empty
		// SubscriptionID, and default the billing window to the clock so the
		// NOT NULL period columns get sane values.
		inv, err := svc.Create(ctx, "t1", CreateInput{CustomerID: "c", Currency: "USD"})
		if err != nil {
			t.Fatalf("expected one-off create to succeed, got %v", err)
		}
		if inv.SubscriptionID != "" {
			t.Errorf("subscription_id: got %q, want empty", inv.SubscriptionID)
		}
		if inv.BillingPeriodStart.IsZero() || inv.BillingPeriodEnd.IsZero() {
			t.Errorf("billing period must default to clock now when omitted")
		}
	})
}

func TestFinalizeAndVoid(t *testing.T) {
	svc := NewService(newMemStore(), nil, newMemNumberer())
	ctx := context.Background()

	inv, _ := svc.Create(ctx, "t1", CreateInput{
		CustomerID: "c", SubscriptionID: "s",
		BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
	})

	t.Run("finalize draft", func(t *testing.T) {
		finalized, err := svc.Finalize(ctx, "t1", inv.ID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if finalized.Status != domain.InvoiceFinalized {
			t.Errorf("got status %q, want finalized", finalized.Status)
		}
		// Industry-standard hosted_invoice_url: finalize must mint a public
		// token so downstream email CTAs (T0-16) have a target on day one.
		if finalized.PublicToken == "" {
			t.Error("finalize should mint a public_token")
		}
		if !strings.HasPrefix(finalized.PublicToken, PublicTokenPrefix) {
			t.Errorf("public_token %q missing %q prefix", finalized.PublicToken, PublicTokenPrefix)
		}
		// Prefix + 64 hex chars of 32-byte entropy.
		if got, want := len(finalized.PublicToken), len(PublicTokenPrefix)+64; got != want {
			t.Errorf("public_token length = %d, want %d", got, want)
		}
	})

	t.Run("cannot finalize again", func(t *testing.T) {
		_, err := svc.Finalize(ctx, "t1", inv.ID)
		if err == nil {
			t.Fatal("expected error finalizing non-draft")
		}
	})

	t.Run("draft has no public_token", func(t *testing.T) {
		draft, _ := svc.Create(ctx, "t1", CreateInput{
			CustomerID: "c", SubscriptionID: "s2",
			BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
		})
		if draft.PublicToken != "" {
			t.Errorf("draft invoice should not carry a public_token, got %q", draft.PublicToken)
		}
	})

	t.Run("rotate public token on finalized invoice", func(t *testing.T) {
		// inv is already finalized from the earlier subtest; rotate
		// should replace the token cleanly.
		current, _ := svc.Get(ctx, "t1", inv.ID)
		original := current.PublicToken
		if original == "" {
			t.Fatal("precondition: finalized invoice should have a token")
		}
		newToken, err := GeneratePublicToken()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if err := svc.SetPublicToken(ctx, "t1", inv.ID, newToken); err != nil {
			t.Fatalf("SetPublicToken: %v", err)
		}
		after, _ := svc.Get(ctx, "t1", inv.ID)
		if after.PublicToken == original {
			t.Error("SetPublicToken should have replaced the token")
		}
		if after.PublicToken != newToken {
			t.Errorf("after = %q, want %q", after.PublicToken, newToken)
		}
	})

	t.Run("rotate rejected on draft invoice", func(t *testing.T) {
		draft, _ := svc.Create(ctx, "t1", CreateInput{
			CustomerID: "c", SubscriptionID: "s3",
			BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
		})
		err := svc.SetPublicToken(ctx, "t1", draft.ID, "hc_pinv_abc")
		if !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("expected ErrNotFound on draft rotate, got %v", err)
		}
	})

	t.Run("void finalized", func(t *testing.T) {
		voided, err := svc.Void(ctx, "t1", inv.ID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if voided.Status != domain.InvoiceVoided {
			t.Errorf("got status %q, want voided", voided.Status)
		}
	})
}

// fakeTaxReverser records ReverseTax calls so the void-tax-reverse
// integration can be asserted at unit-test level without a real
// provider.
type fakeTaxReverser struct {
	calls   []tax.ReversalRequest
	failErr error
	// empty returns a result naming no transaction, with no error: what the
	// engine answers when it resolves no provider.
	empty bool
}

func (f *fakeTaxReverser) ReverseTax(_ context.Context, _ string, req tax.ReversalRequest) (*tax.ReversalResult, error) {
	f.calls = append(f.calls, req)
	if f.failErr != nil {
		return nil, f.failErr
	}
	if f.empty {
		return &tax.ReversalResult{}, nil
	}
	return &tax.ReversalResult{TransactionID: "tax_rev_" + req.Reference}, nil
}

// TestVoid_ReversesUpstreamTaxTransaction locks in the contract that
// voiding a finalized-but-unpaid invoice with a committed tax_transaction
// fires a full-mode upstream reversal. Without this, the authority
// keeps reporting the tax as collected even though the invoice was
// annulled — Stripe Tax docs: "you must reverse the corresponding tax
// transaction to keep your tax records accurate."
func TestVoid_ReversesUpstreamTaxTransaction(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, nil, newMemNumberer())
	rev := &fakeTaxReverser{}
	svc.SetTaxReverser(rev)
	ctx := context.Background()

	inv, _ := svc.Create(ctx, "t1", CreateInput{
		CustomerID: "c", SubscriptionID: "s",
		BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
	})
	if _, err := svc.Finalize(ctx, "t1", inv.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	// Simulate a committed Stripe tax_transaction on the invoice (the
	// production path stamps this from billing.Engine.CommitTax).
	stale := store.invoices[inv.ID]
	stale.TaxTransactionID = "tx_committed_at_finalize"
	store.invoices[inv.ID] = stale

	t.Run("reverses tax with full-mode + invoice-scoped Reference", func(t *testing.T) {
		voided, err := svc.Void(ctx, "t1", inv.ID)
		if err != nil {
			t.Fatalf("Void: %v", err)
		}
		if voided.Status != domain.InvoiceVoided {
			t.Errorf("status: got %q want voided", voided.Status)
		}
		if len(rev.calls) != 1 {
			t.Fatalf("ReverseTax calls: got %d want 1", len(rev.calls))
		}
		call := rev.calls[0]
		if call.OriginalTransactionID != "tx_committed_at_finalize" {
			t.Errorf("OriginalTransactionID: got %q want tx_committed_at_finalize", call.OriginalTransactionID)
		}
		if call.Mode != tax.ReversalModeFull {
			t.Errorf("Mode: got %q want full", call.Mode)
		}
		expectedRef := "inv_taxrev_" + inv.ID
		if call.Reference != expectedRef {
			t.Errorf("Reference: got %q want %q (invoice-stable reversal ref, idempotent per invoice)", call.Reference, expectedRef)
		}
	})

	t.Run("upstream failure still voids locally (best-effort)", func(t *testing.T) {
		inv2, _ := svc.Create(ctx, "t1", CreateInput{
			CustomerID: "c", SubscriptionID: "s2",
			BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
		})
		if _, err := svc.Finalize(ctx, "t1", inv2.ID); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		stale2 := store.invoices[inv2.ID]
		stale2.TaxTransactionID = "tx_committed_at_finalize_2"
		store.invoices[inv2.ID] = stale2
		rev.failErr = errors.New("stripe: tax authority unreachable")
		defer func() { rev.failErr = nil }()

		voided, err := svc.Void(ctx, "t1", inv2.ID)
		if err != nil {
			t.Fatalf("Void should not surface upstream tax errors: %v", err)
		}
		if voided.Status != domain.InvoiceVoided {
			t.Errorf("status: got %q want voided (best-effort tax reversal)", voided.Status)
		}
	})

	t.Run("no upstream call when invoice has no tax transaction", func(t *testing.T) {
		// Manual / none provider: TaxTransactionID empty → reversal skipped.
		inv3, _ := svc.Create(ctx, "t1", CreateInput{
			CustomerID: "c", SubscriptionID: "s3",
			BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
		})
		if _, err := svc.Finalize(ctx, "t1", inv3.ID); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		before := len(rev.calls)
		if _, err := svc.Void(ctx, "t1", inv3.ID); err != nil {
			t.Fatalf("Void: %v", err)
		}
		if len(rev.calls) != before {
			t.Errorf("ReverseTax should not be called when TaxTransactionID is empty; got %d new calls", len(rev.calls)-before)
		}
	})
}

// TestRecordPayment was deleted with the methods it covered (ADR-107): two
// unguarded payment_status writers with no production callers.

// captureAuditInvoice lets the tests assert that the service-layer audit
// rows are written on state transitions. The webhook events are queued by
// the store, and outbox_in_tx_integration_test.go checks them.
type captureAuditInvoice struct {
	entries []capturedAuditEntryInvoice
}

// LogInTx captures like Log — a nil tx is fine for content-level tests;

func (c *captureAuditInvoice) LogInTx(_ context.Context, _ *sql.Tx, e audit.Entry) error {
	c.entries = append(c.entries, capturedAuditEntryInvoice{
		action: e.Action, resourceID: e.ResourceID, resourceType: e.ResourceType, metadata: e.Metadata,
	})
	return nil
}

type capturedAuditEntryInvoice struct {
	action       string
	resourceID   string
	resourceType string
	metadata     map[string]any
}

func (c *captureAuditInvoice) Log(_ context.Context, _, action, resourceType, resourceID, _ string, metadata map[string]any) error {
	c.entries = append(c.entries, capturedAuditEntryInvoice{
		action: action, resourceType: resourceType, resourceID: resourceID, metadata: metadata,
	})
	return nil
}

// TestMarkUncollectible_ReversesUpstreamTaxTransaction mirrors the
// void path: finalize commits a tax_transaction; marking the invoice
// uncollectible must reverse it so the authority's records stop
// reporting the tax as collected. Stripe Tax docs explicitly mention
// both void and uncollectible.
func TestVoid_PartialTaxReversalAfterPriorCreditNote(t *testing.T) {
	// Bug #5 regression: a finalized-unpaid invoice that already had a credit
	// note issued (which reversed its own tax slice via ModePartial) must, on
	// Void, reverse only the REMAINING un-credit-noted tax — not the whole
	// transaction again. An unconditional ModeFull would double-reverse the
	// already-reversed slice and under-remit the tenant's output tax.
	store := newMemStore()
	svc := NewService(store, nil, newMemNumberer())
	rev := &fakeTaxReverser{}
	svc.SetTaxReverser(rev)
	ctx := context.Background()

	inv, _ := svc.Create(ctx, "t1", CreateInput{
		CustomerID: "c", SubscriptionID: "s",
		BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
	})
	if _, err := svc.Finalize(ctx, "t1", inv.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	// $110 taxed invoice with a $50 adjustment credit note already issued
	// (amount_due 11000 -> 6000), committed upstream tax transaction.
	stale := store.invoices[inv.ID]
	stale.TaxTransactionID = "tx_committed"
	stale.TotalAmountCents = 11000
	stale.AmountDueCents = 6000
	stale.AmountPaidCents = 0
	store.invoices[inv.ID] = stale

	if _, err := svc.Void(ctx, "t1", inv.ID); err != nil {
		t.Fatalf("Void: %v", err)
	}
	if len(rev.calls) != 1 {
		t.Fatalf("ReverseTax calls: got %d want 1", len(rev.calls))
	}
	call := rev.calls[0]
	if call.Mode != tax.ReversalModePartial {
		t.Errorf("Mode: got %q want partial (prior CN already reversed part of the tax)", call.Mode)
	}
	if call.GrossAmountCents != 6000 {
		t.Errorf("GrossAmountCents: got %d want 6000 (remaining un-credit-noted gross, NOT the full 11000)", call.GrossAmountCents)
	}
}

type fakeCreditNoteTotaler struct {
	credited   int64
	unrelieved int64
}

func (f *fakeCreditNoteTotaler) IssuedCreditedCents(_ context.Context, _, _ string) (int64, error) {
	return f.credited, nil
}

func (f *fakeCreditNoteTotaler) UnreliefedClawbackCents(_ context.Context, _, _ string) (int64, error) {
	return f.unrelieved, nil
}

// TestVoid_CreditApplied_TaxNotUnderReversed is the audit's void-tax regression:
// applied customer credit reduces amount_due WITHOUT reversing tax, so the old
// amount_paid+amount_due proxy under-reversed tax on void. With the credit-note
// totaler wired (reporting NO credit note), void must reverse the FULL tax even
// though amount_due is short.
//
// Untouched by ADR-111 — that decision removed the write-off's tax leg and left
// VOID's alone. It was deleted by mistake alongside the uncollectible tests and
// restored when golangci-lint flagged its now-unused fake; the fake was the only
// evidence left that the test had gone.
func TestVoid_CreditApplied_TaxNotUnderReversed(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, nil, newMemNumberer())
	rev := &fakeTaxReverser{}
	svc.SetTaxReverser(rev)
	svc.SetCreditNoteTotaler(&fakeCreditNoteTotaler{credited: 0}) // no credit note exists
	ctx := context.Background()

	inv, _ := svc.Create(ctx, "t1", CreateInput{
		CustomerID: "c", SubscriptionID: "s",
		BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
	})
	if _, err := svc.Finalize(ctx, "t1", inv.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	// $110 taxed invoice, $30 paid down by APPLIED CUSTOMER CREDIT (not a credit
	// note): amount_due 8000, amount_paid 0, no credit note. The credit didn't
	// reverse any tax, so the whole $110's tax must be reversed on void.
	stale := store.invoices[inv.ID]
	stale.TaxTransactionID = "tx_committed"
	stale.TotalAmountCents = 11000
	stale.AmountDueCents = 8000
	stale.AmountPaidCents = 0
	store.invoices[inv.ID] = stale

	if _, err := svc.Void(ctx, "t1", inv.ID); err != nil {
		t.Fatalf("Void: %v", err)
	}
	if len(rev.calls) != 1 {
		t.Fatalf("ReverseTax calls: got %d want 1", len(rev.calls))
	}
	// remaining = total - credited(0) = 11000 = total → FULL reversal. The old
	// proxy would have done ModePartial(8000), under-reversing $30 of tax.
	if rev.calls[0].Mode != tax.ReversalModeFull {
		t.Errorf("Mode: got %q want full (applied credit must NOT shrink the tax reversal)", rev.calls[0].Mode)
	}
}

// TestMarkUncollectible_DoesNotReverseTax is the ADR-111 decision, inverted
// from the test it replaces.
//
// A write-off changes who PAID, not what was SOLD, so it has no tax leg. The
// old test asserted the opposite and passed for months — the behaviour it
// pinned shipped a machine-driven under-remit: park an invoice, write it off
// (its only legal exit), the reversal fires, then the ADR-108 search finds the
// charge succeeded and settles it PAID with its tax un-reported.
//
// The negative control below is the load-bearing half: VOID must still
// reverse. Without it, a fix that simply disabled reverseInvoiceTax entirely
// would pass — and that would strand real tax on annulled sales.
func TestMarkUncollectible_DoesNotReverseTax(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, nil, newMemNumberer())
	rev := &fakeTaxReverser{}
	svc.SetTaxReverser(rev)
	ctx := context.Background()

	seed := func() string {
		inv, _ := svc.Create(ctx, "t1", CreateInput{
			CustomerID: "c", SubscriptionID: "s",
			BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
		})
		if _, err := svc.Finalize(ctx, "t1", inv.ID); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		stale := store.invoices[inv.ID]
		stale.TaxTransactionID = "tx_committed_at_finalize"
		store.invoices[inv.ID] = stale
		return inv.ID
	}

	writtenOff := seed()
	if _, err := svc.MarkUncollectible(ctx, "t1", writtenOff); err != nil {
		t.Fatalf("MarkUncollectible: %v", err)
	}
	if len(rev.calls) != 0 {
		t.Fatalf("a write-off reversed tax (%d calls) — it has no tax leg; the sale still happened and the tax was correctly reported (ADR-111)", len(rev.calls))
	}

	// Negative control: void DOES reverse — the supply is annulled.
	voided := seed()
	if _, err := svc.Void(ctx, "t1", voided); err != nil {
		t.Fatalf("Void: %v", err)
	}
	if len(rev.calls) != 1 {
		t.Fatalf("VOID reversal calls: got %d want 1 — voiding annuls the supply and must still reverse", len(rev.calls))
	}
	if rev.calls[0].OriginalTransactionID != "tx_committed_at_finalize" {
		t.Errorf("OriginalTransactionID: got %q", rev.calls[0].OriginalTransactionID)
	}
	if rev.calls[0].Mode != tax.ReversalModeFull {
		t.Errorf("Mode: got %q want full", rev.calls[0].Mode)
	}
}

// TestUncollectibleThenVoid_ReversesExactlyOnce keeps the DECISION of the
// product-audit G1 regression it replaces — a written-off invoice that is later
// voided must never double-reverse its tax (which would UNDER-remit output tax)
// — and updates the mechanism.
//
// Before ADR-111 that took two reversal calls sharing an invoice-stable
// reference, relying on Stripe to dedup them. Now the write-off has no tax leg
// at all, so exactly one call is made and the guarantee is STRUCTURAL rather
// than provider-dependent: there is no second call to dedup.
//
// The stable reference is still asserted, because it is what makes a RETRY of
// the void-path reversal converge (RetryPendingTaxReversal re-drives it).
func TestUncollectibleThenVoid_ReversesExactlyOnce(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, nil, newMemNumberer())
	rev := &fakeTaxReverser{}
	svc.SetTaxReverser(rev)
	ctx := context.Background()

	inv, _ := svc.Create(ctx, "t1", CreateInput{
		CustomerID: "c", SubscriptionID: "s",
		BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
	})
	if _, err := svc.Finalize(ctx, "t1", inv.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	stale := store.invoices[inv.ID]
	stale.TaxTransactionID = "tx_committed"
	stale.TotalAmountCents = 11000
	stale.AmountDueCents = 11000
	stale.AmountPaidCents = 0
	store.invoices[inv.ID] = stale

	if _, err := svc.MarkUncollectible(ctx, "t1", inv.ID); err != nil {
		t.Fatalf("MarkUncollectible: %v", err)
	}
	if len(rev.calls) != 0 {
		t.Fatalf("the write-off reversed tax (%d calls) — ADR-111: no tax leg", len(rev.calls))
	}

	if _, err := svc.Void(ctx, "t1", inv.ID); err != nil {
		t.Fatalf("Void of an uncollectible invoice should be allowed: %v", err)
	}
	if len(rev.calls) != 1 {
		t.Fatalf("expected exactly 1 ReverseTax call (the void), got %d — more than one under-remits by reversing the same tax twice", len(rev.calls))
	}
	if want := "inv_taxrev_" + inv.ID; rev.calls[0].Reference != want {
		t.Errorf("reversal reference must be invoice-stable so a RETRY converges: got %q want %q", rev.calls[0].Reference, want)
	}
	if rev.calls[0].OriginalTransactionID != "tx_committed" {
		t.Errorf("OriginalTransactionID: got %q want tx_committed", rev.calls[0].OriginalTransactionID)
	}
}

// TestMarkUncollectible_WritesAudit locks in the audit row of a write-off,
// so Activity shows the transition. It is written in the service, so every
// caller (HTTP handler, dunning final-action adapter, ResolveRun cross-flow)
// gets it. The store queues invoice.marked_uncollectible
// (TestMarkUncollectible_ProcessStopsAfterCommit_EventIsQueued).
func TestMarkUncollectible_WritesAudit(t *testing.T) {
	svc := NewService(newMemStore(), nil, newMemNumberer())
	audit := &captureAuditInvoice{}
	svc.SetAuditLogger(audit)
	ctx := context.Background()

	inv, _ := svc.Create(ctx, "t1", CreateInput{
		CustomerID: "c", SubscriptionID: "s",
		BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
	})
	if _, err := svc.Finalize(ctx, "t1", inv.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	out, err := svc.MarkUncollectible(ctx, "t1", inv.ID)
	if err != nil {
		t.Fatalf("MarkUncollectible: %v", err)
	}
	if out.Status != domain.InvoiceUncollectible {
		t.Errorf("status: got %q, want uncollectible", out.Status)
	}
	// Three rows — the fixture builds the invoice THROUGH the service, and
	// both of its steps are audited paths in their own right: Create emits the
	// manual-create row (ADR-090, replacing the middleware catch-all's guess),
	// Finalize the canonical finalize row. MarkUncollectible's row is the LAST.
	if len(audit.entries) != 3 {
		t.Fatalf("audit entries: got %d, want 3 (create + finalize + marked_uncollectible)", len(audit.entries))
	}
	if a := audit.entries[0]; a.action != string(domain.AuditActionCreate) {
		t.Errorf("first audit action: got %v, want create", a.action)
	}
	if a := audit.entries[1]; a.action != string(domain.AuditActionFinalize) {
		t.Errorf("second audit action: got %v, want finalize", a.action)
	}
	if a := audit.entries[2]; a.metadata["action"] != "marked_uncollectible" {
		t.Errorf("audit metadata.action: got %v, want marked_uncollectible", a.metadata["action"])
	}
}

// TestRecordOfflinePayment exercises the Stripe-parity offline-recovery
// transition: finalized-unpaid → paid AND uncollectible → paid. Both
// must (a) succeed and (b) write audit with the originating status in
// metadata. Voided + already-paid are rejected. The store queues
// invoice.payment_recorded
// (TestRecordOfflinePayment_ContextEndsAfterCommit_EventIsQueued).
func TestRecordOfflinePayment(t *testing.T) {
	mkSvc := func() (*Service, *captureAuditInvoice) {
		svc := NewService(newMemStore(), nil, newMemNumberer())
		audit := &captureAuditInvoice{}
		svc.SetAuditLogger(audit)
		return svc, audit
	}
	ctx := context.Background()
	mkFinalized := func(svc *Service) domain.Invoice {
		inv, _ := svc.Create(ctx, "t1", CreateInput{
			CustomerID: "c", SubscriptionID: "s",
			BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
		})
		final, _ := svc.Finalize(ctx, "t1", inv.ID)
		return final
	}

	t.Run("finalized → paid", func(t *testing.T) {
		svc, audit := mkSvc()
		inv := mkFinalized(svc)
		out, err := svc.RecordOfflinePayment(ctx, "t1", inv.ID, "Cheque #1234")
		if err != nil {
			t.Fatalf("RecordOfflinePayment: %v", err)
		}
		if out.PaymentStatus != domain.PaymentSucceeded {
			t.Errorf("payment_status: got %q, want succeeded", out.PaymentStatus)
		}
		if out.PaidAt == nil {
			t.Error("paid_at should be set")
		}
		// The fixture builds the invoice through the service, so entries[0] is
		// the manual-create row (ADR-090) and entries[1] the canonical finalize
		// row; entries[2] is the payment_recorded row under test.
		if len(audit.entries) != 3 || audit.entries[2].metadata["action"] != "payment_recorded" {
			t.Fatalf("audit: got %+v, want create + finalize + payment_recorded", audit.entries)
		}
		if audit.entries[2].metadata["recovered_from_status"] != string(domain.InvoiceFinalized) {
			t.Errorf("audit recovered_from_status: got %v, want finalized", audit.entries[2].metadata["recovered_from_status"])
		}
		if audit.entries[2].metadata["note"] != "Cheque #1234" {
			t.Errorf("audit note: got %v, want Cheque #1234", audit.entries[2].metadata["note"])
		}
	})

	t.Run("uncollectible → paid (Stripe recovery)", func(t *testing.T) {
		svc, _ := mkSvc()
		inv := mkFinalized(svc)
		if _, err := svc.MarkUncollectible(ctx, "t1", inv.ID); err != nil {
			t.Fatalf("setup mark uncollectible: %v", err)
		}
		out, err := svc.RecordOfflinePayment(ctx, "t1", inv.ID, "")
		if err != nil {
			t.Fatalf("RecordOfflinePayment from uncollectible: %v", err)
		}
		if out.Status != domain.InvoicePaid {
			t.Errorf("status: got %q, want paid (recovery from uncollectible)", out.Status)
		}
	})

	t.Run("rejects already paid", func(t *testing.T) {
		svc, _ := mkSvc()
		inv := mkFinalized(svc)
		if _, err := svc.RecordOfflinePayment(ctx, "t1", inv.ID, ""); err != nil {
			t.Fatalf("first record: %v", err)
		}
		if _, err := svc.RecordOfflinePayment(ctx, "t1", inv.ID, ""); err == nil {
			t.Error("expected error on already-paid invoice")
		}
	})

	t.Run("rejects voided", func(t *testing.T) {
		svc, _ := mkSvc()
		inv := mkFinalized(svc)
		if _, err := svc.Void(ctx, "t1", inv.ID); err != nil {
			t.Fatalf("setup void: %v", err)
		}
		if _, err := svc.RecordOfflinePayment(ctx, "t1", inv.ID, ""); err == nil {
			t.Error("expected error on voided invoice")
		}
	})

	t.Run("rejects draft", func(t *testing.T) {
		svc, _ := mkSvc()
		inv, _ := svc.Create(ctx, "t1", CreateInput{
			CustomerID: "c", SubscriptionID: "s",
			BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
		})
		if _, err := svc.RecordOfflinePayment(ctx, "t1", inv.ID, ""); err == nil {
			t.Error("expected error on draft invoice")
		}
	})

	// The audit row keeps 500 bytes of the note. When that cut lands inside
	// a character, the row keeps the note up to the character before it,
	// never half of it.
	t.Run("a long note is cut between two characters", func(t *testing.T) {
		svc, audit := mkSvc()
		inv := mkFinalized(svc)
		kept := strings.Repeat("a", 499)
		if _, err := svc.RecordOfflinePayment(ctx, "t1", inv.ID, kept+"é and more"); err != nil {
			t.Fatalf("RecordOfflinePayment: %v", err)
		}
		meta := audit.entries[len(audit.entries)-1].metadata
		if meta["note"] != kept {
			t.Errorf("audit note = %q, want the first 499 bytes, which end before the split character", meta["note"])
		}
		if meta["note_truncated"] != true {
			t.Errorf("audit note_truncated = %v, want true", meta["note_truncated"])
		}
	})

	// amount_cents must be the BOOKED amount, not the post-transition
	// amount_due. MarkPaid flips amount_paid=amount_due and amount_due=0
	// in one statement, so AmountDueCents on the updated row is 0. The
	// event's amounts are checked against the store
	// (TestRecordOfflinePayment_ContextEndsAfterCommit_EventIsQueued).
	t.Run("audit carries the booked amount, not zero", func(t *testing.T) {
		store := newMemStore()
		store.invoices["inv_amt"] = domain.Invoice{
			ID: "inv_amt", TenantID: "t1", CustomerID: "c", InvoiceNumber: "HC-42",
			Status: domain.InvoiceFinalized, AmountDueCents: 4200, Currency: "USD",
		}
		svc := NewService(store, nil, newMemNumberer())
		audit := &captureAuditInvoice{}
		svc.SetAuditLogger(audit)

		if _, err := svc.RecordOfflinePayment(ctx, "t1", "inv_amt", "Wire 2026-07-05"); err != nil {
			t.Fatalf("RecordOfflinePayment: %v", err)
		}
		if len(audit.entries) != 1 || audit.entries[0].metadata["amount_cents"] != int64(4200) {
			t.Errorf("audit amount_cents: got %v, want 4200 (booked amount, not the zeroed amount_due)",
				audit.entries[0].metadata["amount_cents"])
		}
	})
}

// TestMarkPaid_StoreGate_RejectsDraftAndTaxPending locks in the
// 2026-05-22 store-level invariant. The credit-fully-paid path in
// billOnePeriod (and threshold_scan) previously called MarkPaid on
// draft / tax-pending invoices, locking the customer at subtotal-only
// totals. The store-level gate is the canonical guard; the engine
// caller-level gates are defense-in-depth. This test exercises the
// store gate via the in-memory MarkPaid (which mirrors PostgresStore's
// gate).
func TestMarkPaid_StoreGate_RejectsDraftAndTaxPending(t *testing.T) {
	store := newMemStore()
	ctx := context.Background()

	now := time.Date(2027, 9, 19, 18, 30, 0, 0, time.UTC)

	t.Run("rejects draft (finalize first)", func(t *testing.T) {
		// Synthesize a draft invoice with tax_status=ok (so the failure
		// is purely the draft status gate, not the tax gate).
		store.invoices["inv_draft"] = domain.Invoice{
			ID: "inv_draft", TenantID: "t1",
			Status: domain.InvoiceDraft, TaxFacts: domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK},
		}
		_, err := store.MarkPaid(ctx, "t1", "inv_draft", "", now)
		if err == nil {
			t.Error("expected MarkPaid on draft invoice to fail")
		}
	})

	t.Run("rejects tax_status=pending on finalized", func(t *testing.T) {
		// Even a finalized invoice — if tax somehow flipped back to
		// pending — must not transition to paid. Synthetic since the
		// natural path keeps these in sync, but the gate is a hard
		// invariant against future refactors.
		store.invoices["inv_finalized_tax_pending"] = domain.Invoice{
			ID: "inv_finalized_tax_pending", TenantID: "t1",
			Status: domain.InvoiceFinalized, TaxFacts: domain.TaxFacts{TaxStatus: domain.InvoiceTaxPending},
		}
		_, err := store.MarkPaid(ctx, "t1", "inv_finalized_tax_pending", "", now)
		if err == nil {
			t.Error("expected MarkPaid with tax_status=pending to fail")
		}
	})

	t.Run("rejects voided (terminal)", func(t *testing.T) {
		store.invoices["inv_voided"] = domain.Invoice{
			ID: "inv_voided", TenantID: "t1",
			Status: domain.InvoiceVoided, TaxFacts: domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK},
		}
		_, err := store.MarkPaid(ctx, "t1", "inv_voided", "", now)
		if err == nil {
			t.Error("expected MarkPaid on voided invoice to fail")
		}
	})

	t.Run("allows finalized + tax_status=ok", func(t *testing.T) {
		store.invoices["inv_ok"] = domain.Invoice{
			ID: "inv_ok", TenantID: "t1",
			Status: domain.InvoiceFinalized, TaxFacts: domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK},
			AmountDueCents: 7000,
		}
		out, err := store.MarkPaid(ctx, "t1", "inv_ok", "", now)
		if err != nil {
			t.Fatalf("MarkPaid: %v", err)
		}
		if out.Status != domain.InvoicePaid {
			t.Errorf("status: got %q, want paid", out.Status)
		}
	})

	t.Run("allows uncollectible (Stripe-parity recovery)", func(t *testing.T) {
		store.invoices["inv_unc"] = domain.Invoice{
			ID: "inv_unc", TenantID: "t1",
			Status: domain.InvoiceUncollectible, TaxFacts: domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK},
		}
		_, err := store.MarkPaid(ctx, "t1", "inv_unc", "", now)
		if err != nil {
			t.Errorf("MarkPaid on uncollectible should succeed: %v", err)
		}
	})

	t.Run("idempotent on already-paid", func(t *testing.T) {
		store.invoices["inv_paid"] = domain.Invoice{
			ID: "inv_paid", TenantID: "t1",
			Status: domain.InvoicePaid, TaxFacts: domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK},
		}
		_, err := store.MarkPaid(ctx, "t1", "inv_paid", "", now)
		if err != nil {
			t.Errorf("idempotent re-MarkPaid should succeed: %v", err)
		}
	})
}

// recordingTaxCommitter captures CommitTax calls for the reconciler test.
type recordingTaxCommitter struct {
	calls []string // invoice ids
	err   error
}

// The fake's check always passes — expiry behavior is pinned by the
// engine's own tests plus TestFinalize_ExpiredTaxCalculationBlocks, which
// uses a fake that returns the expiry error.
func (r *recordingTaxCommitter) CheckTaxCalculationUsable(context.Context, string, string, string) error {
	return nil
}

func (r *recordingTaxCommitter) CommitTax(_ context.Context, _, invoiceID, _ string) error {
	r.calls = append(r.calls, invoiceID)
	return r.err
}

// TestRetryPendingTaxCommit verifies the commit reconciler re-commits only the
// orphan invoices — finalized stripe_tax with a calculation id but no
// transaction id — and leaves already-committed / non-stripe invoices alone.
func TestRetryPendingTaxCommit(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, nil, nil)
	committer := &recordingTaxCommitter{}
	svc.SetTaxCommitter(committer)
	ctx := postgres.WithLivemode(context.Background(), true)

	// The orphan: commit succeeded at Stripe, local tax_transaction_id lost.
	store.invoices["orphan"] = domain.Invoice{
		ID: "orphan", TenantID: "t1", Status: domain.InvoiceFinalized,
		TaxFacts: domain.TaxFacts{
			TaxStatus: domain.InvoiceTaxOK, TaxProvider: "stripe_tax",
			TaxCalculationID: "taxcalc_1",
		},
		TaxTransactionID: "",
	}
	// Already committed — excluded.
	store.invoices["committed"] = domain.Invoice{
		ID: "committed", TenantID: "t1", Status: domain.InvoiceFinalized,
		TaxFacts: domain.TaxFacts{
			TaxStatus: domain.InvoiceTaxOK, TaxProvider: "stripe_tax",
			TaxCalculationID: "taxcalc_2",
		},
		TaxTransactionID: "tx_2",
	}
	// Manual provider (no calc id, nothing to commit) — excluded.
	store.invoices["manual"] = domain.Invoice{
		ID: "manual", TenantID: "t1", Status: domain.InvoiceFinalized,
		TaxFacts: domain.TaxFacts{
			TaxStatus: domain.InvoiceTaxOK, TaxProvider: "manual",
		},
	}

	recovered, errs := svc.RetryPendingTaxCommit(ctx, 50)
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if recovered != 1 {
		t.Errorf("recovered = %d, want 1 (only the orphan)", recovered)
	}
	if len(committer.calls) != 1 || committer.calls[0] != "orphan" {
		t.Errorf("CommitTax calls = %v, want exactly [orphan]", committer.calls)
	}
}

// TestRetryTax_AutoFinalize_QueuesForCollection pins the handoff between the
// tax reconciler and the auto-charge sweep. RetryTax auto-finalizes an engine
// invoice the moment tax resolves — but finalizing is not collecting: nothing
// charges the card, starts dunning, or emails the customer. The ONLY thing
// that re-drives collection afterwards is auto_charge_pending, which is what
// both sweeps list on (ListAutoChargePending / ListAutoChargePendingForClock).
//
// Without the flag the invoice is invisible to every retry owner: the charge
// sweeps skip it (flag false), the dunning backfill skips it (payment_status
// is 'pending', not 'failed'), and dunning never started because nothing ever
// failed. It sits finalized and owed until an operator notices the banner —
// the same silent-overdue sink already fixed for the manual-finalize path.
//
// The catchup pipeline encodes the intended handoff in its phase ORDER: tax
// retry (Phase 2) runs before auto-charge (Phase 3) specifically so an
// unstuck invoice charges in the same Advance. That ordering only means
// something if this flag is set.
func TestRetryTax_AutoFinalize_QueuesForCollection(t *testing.T) {
	store := newMemStore()
	now := time.Now().UTC()

	store.invoices["inv_stuck"] = domain.Invoice{
		ID: "inv_stuck", TenantID: "t1", CustomerID: "cus_a",
		Status: domain.InvoiceDraft, PaymentStatus: domain.PaymentPending,
		TaxFacts: domain.TaxFacts{
			TaxStatus: domain.InvoiceTaxPending, TaxErrorCode: "provider_outage",
		},
		BillingReason:    "subscription_cycle",
		Currency:         "USD",
		TotalAmountCents: 5000,
		AmountDueCents:   5000,
		CreatedAt:        now,
	}

	svc := NewService(store, nil, nil)
	svc.SetTaxRetrier(&stubTaxRetrier{store: store})

	out, err := svc.RetryTax(context.Background(), "t1", "inv_stuck")
	if err != nil {
		t.Fatalf("RetryTax: %v", err)
	}
	if out.Status != domain.InvoiceFinalized {
		t.Fatalf("status = %s, want finalized (the auto-finalize precondition)", out.Status)
	}
	if out.AmountDueCents <= 0 {
		t.Fatalf("amount_due = %d, want > 0 (the zero-due arm settles paid instead)", out.AmountDueCents)
	}

	stored := store.invoices["inv_stuck"]
	if !stored.AutoChargePending {
		t.Error("auto_charge_pending = false after auto-finalize: no sweep will ever collect this invoice")
	}
}

// stubPauseReader answers the pause lookup the attention banners and Create
// use. The tax retry's auto-finalize reads the pause on its transaction
// (memStore.CollectionPausedTx).
type stubPauseReader struct {
	paused bool
	err    error
	calls  int
}

func (s *stubPauseReader) Get(_ context.Context, _, id string) (domain.Subscription, error) {
	s.calls++
	if s.err != nil {
		return domain.Subscription{}, s.err
	}
	sub := domain.Subscription{ID: id}
	if s.paused {
		sub.PauseCollection = &domain.PauseCollection{Behavior: domain.PauseCollectionKeepAsDraft}
	}
	return sub, nil
}

// PauseCollections mirrors the real store: a missing subscription is absent
// from the map (the stub's ErrNotFound), any other error is returned.
func (s *stubPauseReader) PauseCollections(_ context.Context, _ string, ids []string) (map[string]*domain.PauseCollection, error) {
	s.calls++
	out := map[string]*domain.PauseCollection{}
	if errors.Is(s.err, errs.ErrNotFound) {
		return out, nil
	}
	if s.err != nil {
		return nil, s.err
	}
	for _, id := range ids {
		if s.paused {
			out[id] = &domain.PauseCollection{Behavior: domain.PauseCollectionKeepAsDraft}
		} else {
			out[id] = nil
		}
	}
	return out, nil
}

// TestRetryTax_AutoFinalize_RespectsPauseCollection pins that a paused
// subscription's draft stays a draft when its tax resolves.
//
// A tax-deferred invoice on a paused sub is draft for two independent reasons;
// resolving the tax clears only one. Auto-finalizing on that would issue an
// invoice the operator asked us not to send — and since auto-finalize now
// queues for collection, its banner would promise an automatic charge that the
// sweeps (correctly) refuse to make. keep_as_draft means keep it as a draft.
func TestRetryTax_AutoFinalize_RespectsPauseCollection(t *testing.T) {
	newStuckInvoice := func(store *memStore) {
		store.invoices["inv_paused"] = domain.Invoice{
			ID: "inv_paused", TenantID: "t1", CustomerID: "cus_a",
			SubscriptionID: "sub_1",
			Status:         domain.InvoiceDraft,
			PaymentStatus:  domain.PaymentPending,
			TaxFacts: domain.TaxFacts{
				TaxStatus: domain.InvoiceTaxPending, TaxErrorCode: "provider_outage",
			},
			BillingReason:    "subscription_cycle",
			Currency:         "USD",
			TotalAmountCents: 5000,
			AmountDueCents:   5000,
			CreatedAt:        time.Now().UTC(),
		}
	}

	t.Run("paused → stays draft, not queued", func(t *testing.T) {
		store := newMemStore()
		newStuckInvoice(store)
		svc := NewService(store, nil, nil)
		svc.SetTaxRetrier(&stubTaxRetrier{store: store})
		store.pausedSubs = map[string]bool{"sub_1": true}

		out, err := svc.RetryTax(context.Background(), "t1", "inv_paused")
		if err != nil {
			t.Fatalf("RetryTax: %v", err)
		}
		if out.Status != domain.InvoiceDraft {
			t.Errorf("status = %s, want draft (pause_collection keeps it a draft)", out.Status)
		}
		if store.invoices["inv_paused"].AutoChargePending {
			t.Error("a paused subscription's invoice must not be queued for collection")
		}
		// The tax recompute itself must still stick — the gate withholds
		// issuance, it does not discard the retry.
		if store.invoices["inv_paused"].TaxStatus != domain.InvoiceTaxOK {
			t.Error("tax retry result must persist even when finalize is withheld")
		}
	})

	t.Run("not paused → finalizes and queues", func(t *testing.T) {
		store := newMemStore()
		newStuckInvoice(store)
		svc := NewService(store, nil, nil)
		svc.SetTaxRetrier(&stubTaxRetrier{store: store})

		out, err := svc.RetryTax(context.Background(), "t1", "inv_paused")
		if err != nil {
			t.Fatalf("RetryTax: %v", err)
		}
		if out.Status != domain.InvoiceFinalized {
			t.Errorf("status = %s, want finalized", out.Status)
		}
		if !store.invoices["inv_paused"].AutoChargePending {
			t.Error("a running subscription's invoice must still be queued (the pause gate over-reached)")
		}
	})

	// The pause is read on the tax write's transaction (P1-29): a failed read
	// rolls the tax write back, so the row is listed again, and RetryTax
	// returns the error so the sweep counts it.
	t.Run("read error → stays a draft with tax pending (never charge on a guess) and RetryTax errors", func(t *testing.T) {
		store := newMemStore()
		newStuckInvoice(store)
		svc := NewService(store, nil, nil)
		svc.SetTaxRetrier(&stubTaxRetrier{store: store})
		store.pauseErr = errors.New("connection reset")

		if _, err := svc.RetryTax(context.Background(), "t1", "inv_paused"); err == nil {
			t.Fatal("RetryTax = nil on an unread pause state, want the error")
		}
		got := store.invoices["inv_paused"]
		if got.Status != domain.InvoiceDraft {
			t.Errorf("status = %s, want draft — an unreadable pause state must not issue and queue a charge", got.Status)
		}
		if got.AutoChargePending {
			t.Error("auto_charge_pending set on an unread pause state")
		}
		if got.TaxStatus != domain.InvoiceTaxPending {
			t.Errorf("tax_status = %s, want pending (the tax write rolled back with the read)", got.TaxStatus)
		}
	})
}

// expiredTaxCommitter is a TaxCommitter whose pre-check reports the calc as
// expired — the shape the engine returns for a build-time calculation older
// than the provider's 24h TTL.
type expiredTaxCommitter struct {
	commits int
}

func (f *expiredTaxCommitter) CheckTaxCalculationUsable(context.Context, string, string, string) error {
	return errs.InvalidState("tax calculation expired (age 25h0m, max 23h0m) — retry tax to refresh, then finalize").
		WithCode("tax_calculation_expired")
}

func (f *expiredTaxCommitter) CommitTax(context.Context, string, string, string) error {
	f.commits++
	return nil
}

// TestFinalize_ExpiredTaxCalculationBlocks pins the walk-found fix: an
// engine-built draft whose build-time calculation has expired must be REFUSED
// at finalize with the operator remedy — not finalized with the commit
// failure swallowed into a log line. Pre-fix, finalize succeeded, the commit
// warn'd, and the invoice ended up collected with tax printed on the PDF but
// never reported to Stripe; the orphan reconciler could never recover it
// because the same age rule blocks its re-commit.
func TestFinalize_ExpiredTaxCalculationBlocks(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, nil, nil)
	committer := &expiredTaxCommitter{}
	svc.SetTaxCommitter(committer)
	ctx := context.Background()

	// Engine-built shape: a cycle draft carrying a successful build-time
	// calculation (the paused-collection sub's invoice is the common way one
	// sits long enough to age).
	store.invoices["draft1"] = domain.Invoice{
		ID: "draft1", TenantID: "t1", Status: domain.InvoiceDraft,
		BillingReason: domain.BillingReasonSubscriptionCycle,
		TaxFacts: domain.TaxFacts{
			TaxStatus: domain.InvoiceTaxOK, TaxProvider: "stripe_tax",
			TaxCalculationID: "taxcalc_aged",
		},
	}

	_, err := svc.Finalize(ctx, "t1", "draft1")
	if !errors.Is(err, errs.ErrInvalidState) {
		t.Fatalf("finalize of an expired-calc draft must refuse with ErrInvalidState, got %v", err)
	}
	if !strings.Contains(err.Error(), "retry tax to refresh, then finalize") {
		t.Fatalf("the refusal must carry the operator remedy, got %v", err)
	}
	// The load-bearing halves: the invoice did NOT transition, and the
	// commit was never attempted against the expired calc.
	if got := store.invoices["draft1"].Status; got != domain.InvoiceDraft {
		t.Fatalf("invoice must stay draft after the refusal, got %s", got)
	}
	if committer.commits != 0 {
		t.Fatalf("CommitTax must not run against an expired calc, ran %d times", committer.commits)
	}
}

// TestFinalize_CommittedCalcSkipsExpiryCheck is the refinement the LIVE walk
// forced: engine-built cycle invoices commit their tax at BUILD time, so the
// common paused-collection draft carries calc + transaction together. An aged
// calc with a committed transaction must NOT block finalize (nothing is left
// to commit) and must NOT re-commit (expiry noise, pointless provider call).
// The first version of the pre-check blocked this shape — sending the
// operator to "retry tax", which would have minted a second calculation
// beside the committed transaction: double-reported tax.
func TestFinalize_CommittedCalcSkipsExpiryCheck(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, nil, nil)
	committer := &expiredTaxCommitter{} // Check would refuse — must never be consulted
	svc.SetTaxCommitter(committer)

	store.invoices["draft2"] = domain.Invoice{
		ID: "draft2", TenantID: "t1", Status: domain.InvoiceDraft,
		BillingReason: domain.BillingReasonSubscriptionCycle,
		TaxFacts: domain.TaxFacts{
			TaxStatus: domain.InvoiceTaxOK, TaxProvider: "stripe_tax",
			TaxCalculationID: "taxcalc_aged",
		},
		TaxTransactionID: "tax_committed_at_build",
	}

	got, err := svc.Finalize(context.Background(), "t1", "draft2")
	if err != nil {
		t.Fatalf("an aged-but-committed calc must not block finalize: %v", err)
	}
	if got.Status != domain.InvoiceFinalized {
		t.Fatalf("want finalized, got %s", got.Status)
	}
	if committer.commits != 0 {
		t.Fatalf("no re-commit for an already-committed calc, ran %d times", committer.commits)
	}
}

// TestServiceReads_SetBillingPeriodDisplay pins the Period row of every
// rendered invoice PDF. The hosted page reads the invoice through
// GetByPublicToken; the downloaded and emailed PDFs read it through
// GetWithLineItems. BuildPDFContext prints BillingPeriodDisplay as it gets
// it, so each read must set it: FormatInclusivePeriod in the invoice's zone,
// which is its own BillingTimezone, or the tenant's when it has none.
//
// The half-open period [May 2 00:00 IST, Jun 1 00:00 IST) reads
// "May 2, 2026 – May 31, 2026" in Kolkata and "May 1, 2026 – May 30, 2026" in
// New York, so a read that used the wrong zone fails too.
//
// Mutation-verify: delete the BillingPeriodDisplay assignment in
// decorateInvoice, and both reads return "" in every case.
func TestServiceReads_SetBillingPeriodDisplay(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	svc := NewService(store, nil, nil)
	svc.SetTenantSettingsReader(dispSettings{tz: "America/New_York"})

	start := time.Date(2026, 5, 1, 18, 30, 0, 0, time.UTC) // May 2 00:00 IST
	end := time.Date(2026, 5, 31, 18, 30, 0, 0, time.UTC)  // Jun 1 00:00 IST

	cases := []struct {
		name        string
		invTZ       string
		wantZone    string
		wantDisplay string
	}{
		{"the invoice's own zone", "Asia/Kolkata", "Asia/Kolkata", "May 2, 2026 – May 31, 2026"},
		{"the tenant's zone when the invoice has none", "", "America/New_York", "May 1, 2026 – May 30, 2026"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := fmt.Sprintf("inv_period_%d", i)
			token := "tok_" + id
			store.invoices[id] = domain.Invoice{
				ID: id, TenantID: "t1", CustomerID: "cus_1",
				Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
				PublicToken: token, BillingTimezone: tc.invTZ,
				BillingPeriodStart: start, BillingPeriodEnd: end,
			}
			want := domain.FormatInclusivePeriod(start, end, domain.LoadLocationOrUTC(tc.wantZone))
			if want != tc.wantDisplay {
				t.Fatalf("FormatInclusivePeriod in %s = %q, want %q", tc.wantZone, want, tc.wantDisplay)
			}

			hosted, _, err := svc.GetByPublicToken(ctx, token)
			if err != nil {
				t.Fatalf("GetByPublicToken: %v", err)
			}
			if hosted.BillingPeriodDisplay != want {
				t.Errorf("GetByPublicToken BillingPeriodDisplay = %q, want %q", hosted.BillingPeriodDisplay, want)
			}

			fetched, _, err := svc.GetWithLineItems(ctx, "t1", id)
			if err != nil {
				t.Fatalf("GetWithLineItems: %v", err)
			}
			if fetched.BillingPeriodDisplay != want {
				t.Errorf("GetWithLineItems BillingPeriodDisplay = %q, want %q", fetched.BillingPeriodDisplay, want)
			}
		})
	}
}

// TestRetryPendingTaxForClock_NoTaxRetrierIsAnError: a clock's tax retry with
// no retrier fails the advance; skipping it would end the advance ready with
// drafts still waiting for tax.
func TestRetryPendingTaxForClock_NoTaxRetrierIsAnError(t *testing.T) {
	svc := NewService(newMemStore(), nil, nil)
	if _, errs := svc.RetryPendingTaxForClock(context.Background(), "t1", "tc_1", 50); len(errs) != 1 {
		t.Errorf("errors = %v, want one: the job cannot retry tax", errs)
	}
}
