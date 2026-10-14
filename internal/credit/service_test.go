package credit

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

type memStore struct {
	entries []domain.CreditLedgerEntry
}

func newMemStore() *memStore {
	return &memStore{}
}

// AppendEntryTx mirrors AppendEntry for tx-aware callers. The fake
// ignores the tx since it's in-memory — the test exercises business
// rules, not transactional atomicity.
func (m *memStore) AppendEntryTx(ctx context.Context, _ *sql.Tx, tenantID string, entry domain.CreditLedgerEntry) (domain.CreditLedgerEntry, error) {
	return m.AppendEntry(ctx, tenantID, entry)
}

// ListGrantSummaries: the in-memory fake mirrors the store's
// remaining = amount - consumed arithmetic over positive entries.
func (m *memStore) ListGrantSummaries(_ context.Context, tenantID, customerID string, includeExhausted bool) ([]GrantSummary, error) {
	var out []GrantSummary
	for _, e := range m.entries {
		if e.TenantID != tenantID || e.CustomerID != customerID || e.AmountCents <= 0 {
			continue
		}
		if !includeExhausted && e.ConsumedCents >= e.AmountCents {
			continue
		}
		out = append(out, GrantSummary{
			ID: e.ID, EntryType: string(e.EntryType), GrantKind: string(e.GrantKind),
			Description: e.Description, AmountCents: e.AmountCents,
			ConsumedCents: e.ConsumedCents, RemainingCents: e.AmountCents - e.ConsumedCents,
			ExpiresAt: e.ExpiresAt, SourceInvoiceID: e.SourceInvoiceID, CreatedAt: e.CreatedAt,
		})
	}
	return out, nil
}

// Relief-pair stubs: the in-tx relief path is proven against real
// Postgres (commit_cn_relief tests); unit fakes don't model row locks.
func (m *memStore) LockCommitGrantTx(_ context.Context, _ *sql.Tx, tenantID, invoiceID string) (commitGrantState, bool, error) {
	for _, e := range m.entries {
		if e.TenantID == tenantID && e.SourceInvoiceID == invoiceID && e.GrantKind == domain.GrantKindCommit {
			return commitGrantState{GrantID: e.ID, CustomerID: e.CustomerID, AmountCents: e.AmountCents, ConsumedCents: e.ConsumedCents}, true, nil
		}
	}
	return commitGrantState{}, false, nil
}

func (m *memStore) RetireCommitSliceForReliefTx(_ context.Context, _ *sql.Tx, tenantID, invoiceID, _ string, slice, _, _ int64) (commitGrantState, error) {
	for id, e := range m.entries {
		if e.TenantID == tenantID && e.SourceInvoiceID == invoiceID && e.GrantKind == domain.GrantKindCommit {
			if e.ConsumedCents+slice > e.AmountCents {
				return commitGrantState{}, ErrCommitInsufficientRemaining
			}
			e.ConsumedCents += slice
			m.entries[id] = e
			return commitGrantState{GrantID: e.ID, CustomerID: e.CustomerID, AmountCents: e.AmountCents, ConsumedCents: e.ConsumedCents, CNRetiredCents: slice}, nil
		}
	}
	return commitGrantState{}, fmt.Errorf("no commit grant for invoice %s", invoiceID)
}

// Audited variants: the memStore has no real tx, so the emission runs with a
// nil *sql.Tx — and to keep SHARED-FATE semantics faithful to the real
// store, an emission error rolls the in-memory write back (the entry list is
// restored) exactly as the aborted Postgres tx would. Content-level
// assertions live in unit tests; the real rollback coupling is additionally
// pinned by the real-Postgres tests in intx_audit_integration_test.go.
func (m *memStore) AppendEntryAudited(ctx context.Context, tenantID string, entry domain.CreditLedgerEntry, emit func(tx *sql.Tx, out domain.CreditLedgerEntry) error) (domain.CreditLedgerEntry, error) {
	before := len(m.entries)
	out, err := m.AppendEntry(ctx, tenantID, entry)
	if err != nil {
		return out, err
	}
	if emit != nil {
		if err := emit(nil, out); err != nil {
			m.entries = m.entries[:before] // shared fate: roll the write back
			return domain.CreditLedgerEntry{}, fmt.Errorf("audit emission: %w", err)
		}
	}
	return out, nil
}

func (m *memStore) AdjustAtomicAudited(ctx context.Context, tenantID, customerID, description string, amountCents int64, emit func(tx *sql.Tx, out domain.CreditLedgerEntry) error) (domain.CreditLedgerEntry, error) {
	before := len(m.entries)
	out, err := m.AdjustAtomic(ctx, tenantID, customerID, description, amountCents)
	if err != nil {
		return out, err
	}
	if emit != nil {
		if err := emit(nil, out); err != nil {
			m.entries = m.entries[:before] // shared fate: roll the write back
			return domain.CreditLedgerEntry{}, fmt.Errorf("audit emission: %w", err)
		}
	}
	return out, nil
}

func (m *memStore) AppendEntry(_ context.Context, tenantID string, entry domain.CreditLedgerEntry) (domain.CreditLedgerEntry, error) {
	// Emulate the proration dedup partial unique index. Without this, tests
	// exercising retry-after-partial-failure paths silently double-insert
	// rows the real DB would have rejected.
	if entry.SourceSubscriptionID != "" && entry.SourcePlanChangedAt != nil {
		for _, e := range m.entries {
			if e.TenantID == tenantID && e.SourceSubscriptionID == entry.SourceSubscriptionID &&
				e.SourcePlanChangedAt != nil && e.SourcePlanChangedAt.Equal(*entry.SourcePlanChangedAt) {
				return domain.CreditLedgerEntry{}, errs.ErrAlreadyExists
			}
		}
	}

	// Emulate idx_credit_ledger_credit_note_dedup (migration 0093):
	// one grant per (tenant, source_credit_note_id).
	if entry.SourceCreditNoteID != "" {
		for _, e := range m.entries {
			if e.TenantID == tenantID && e.SourceCreditNoteID == entry.SourceCreditNoteID {
				return domain.CreditLedgerEntry{}, errs.ErrAlreadyExists
			}
		}
	}

	// Compute balance
	var balance int64
	for _, e := range m.entries {
		if e.CustomerID == entry.CustomerID {
			balance += e.AmountCents
		}
	}
	entry.BalanceAfter = balance + entry.AmountCents
	entry.ID = fmt.Sprintf("vlx_ccl_%d", len(m.entries)+1)
	entry.TenantID = tenantID
	// Honor caller-supplied CreatedAt (mirrors PostgresStore.AppendEntry
	// which lets the credit-expiry path stamp the row at the grant's
	// expires_at, not wall-clock now). Fall back when zero.
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}
	m.entries = append(m.entries, entry)
	return entry, nil
}

func (m *memStore) GetByCreditNoteSource(_ context.Context, tenantID, creditNoteID string) (domain.CreditLedgerEntry, error) {
	for _, e := range m.entries {
		if e.TenantID == tenantID && e.SourceCreditNoteID == creditNoteID {
			return e, nil
		}
	}
	return domain.CreditLedgerEntry{}, errs.ErrNotFound
}

func (m *memStore) GetByProrationSource(_ context.Context, tenantID, subscriptionID, subscriptionItemID string, changeType domain.ItemChangeType, changeAt time.Time) (domain.CreditLedgerEntry, error) {
	for _, e := range m.entries {
		if e.TenantID == tenantID && e.SourceSubscriptionID == subscriptionID &&
			e.SourceSubscriptionItemID == subscriptionItemID &&
			e.SourceChangeType == changeType &&
			e.SourcePlanChangedAt != nil && e.SourcePlanChangedAt.Equal(changeAt) {
			return e, nil
		}
	}
	return domain.CreditLedgerEntry{}, errs.ErrNotFound
}

func (m *memStore) GetSpendableBalance(_ context.Context, _, customerID string, at time.Time) (int64, error) {
	var spendable int64
	for _, e := range m.entries {
		if e.CustomerID != customerID {
			continue
		}
		spendable += e.AmountCents
		if e.AmountCents > 0 && e.ConsumedCents < e.AmountCents && e.ExpiresAt != nil && !e.ExpiresAt.After(at) {
			spendable -= e.AmountCents - e.ConsumedCents
		}
	}
	return spendable, nil
}

func (m *memStore) GetBalance(_ context.Context, _, customerID string) (domain.CreditBalance, error) {
	var b domain.CreditBalance
	b.CustomerID = customerID
	for _, e := range m.entries {
		if e.CustomerID != customerID {
			continue
		}
		b.BalanceCents += e.AmountCents
		switch e.EntryType {
		case domain.CreditGrant:
			b.TotalGranted += e.AmountCents
		case domain.CreditUsage:
			b.TotalUsed += -e.AmountCents
		case domain.CreditExpiry:
			b.TotalExpired += -e.AmountCents
		}
	}
	return b, nil
}

func (m *memStore) ListBalances(_ context.Context, _ string) ([]domain.CreditBalance, error) {
	byCustomer := map[string]*domain.CreditBalance{}
	for _, e := range m.entries {
		b, ok := byCustomer[e.CustomerID]
		if !ok {
			b = &domain.CreditBalance{CustomerID: e.CustomerID}
			byCustomer[e.CustomerID] = b
		}
		b.BalanceCents += e.AmountCents
		switch e.EntryType {
		case domain.CreditGrant:
			b.TotalGranted += e.AmountCents
		case domain.CreditUsage:
			b.TotalUsed += -e.AmountCents
		}
	}
	var result []domain.CreditBalance
	for _, b := range byCustomer {
		result = append(result, *b)
	}
	return result, nil
}

// ExpireGrantAtomic mirrors the real store's semantics: re-read the
// grant from current state (not the caller's snapshot), no-op when
// fully consumed, flip consumed_cents and append the expiry entry
// together. In-memory there is no tx, but the re-read-then-gate shape
// is what the service-level tests exercise.
func (m *memStore) ExpireGrantAtomic(ctx context.Context, tenantID, customerID, grantID string) (int64, error) {
	for i := range m.entries {
		e := &m.entries[i]
		if e.TenantID != tenantID || e.CustomerID != customerID || e.ID != grantID || e.EntryType != domain.CreditGrant {
			continue
		}
		remaining := e.AmountCents - e.ConsumedCents
		if remaining <= 0 {
			return 0, nil
		}
		if e.ExpiresAt == nil {
			return 0, fmt.Errorf("expire grant %s: grant has no expires_at", grantID)
		}
		expiredAt := *e.ExpiresAt
		e.ConsumedCents = e.AmountCents
		if _, err := m.AppendEntry(ctx, tenantID, domain.CreditLedgerEntry{
			CustomerID:  customerID,
			EntryType:   domain.CreditExpiry,
			AmountCents: -remaining,
			Description: fmt.Sprintf("Expired grant %s", grantID),
			CreatedAt:   expiredAt,
		}); err != nil {
			return 0, err
		}
		return remaining, nil
	}
	return 0, errs.ErrNotFound
}

func (m *memStore) ListExpiredGrants(_ context.Context, _ time.Duration) ([]domain.CreditLedgerEntry, error) {
	// Stub: no test exercises ExpireCredits against memStore; integration
	// coverage of expiry lives against PostgresStore. Present only to
	// satisfy the Store interface.
	return nil, nil
}

// ListExpiredGrantsForClock — ADR-029 Phase 4 stub, same rationale as
// the cron variant above (postgres integration tests own the SQL,
// memStore satisfies the interface contract for unit tests).
func (m *memStore) ListExpiredGrantsForClock(_ context.Context, _, _ string, _ time.Time) ([]domain.CreditLedgerEntry, error) {
	return nil, nil
}

func (m *memStore) AdjustAtomic(ctx context.Context, tenantID, customerID, description string, amountCents int64) (domain.CreditLedgerEntry, error) {
	bal, err := m.GetBalance(ctx, tenantID, customerID)
	if err != nil {
		return domain.CreditLedgerEntry{}, err
	}
	if amountCents < 0 && bal.BalanceCents+amountCents < 0 {
		return domain.CreditLedgerEntry{}, fmt.Errorf("insufficient balance: available %.2f, deduction %.2f",
			float64(bal.BalanceCents)/100, float64(-amountCents)/100)
	}
	// Mirror PostgresStore.AdjustAtomic: stamp created_at from the
	// ctx-bound clock so a clock-pinned customer's deduct lands in sim
	// time, not wall-clock. AppendEntry's zero-CreatedAt fallback
	// would otherwise mask the missing bind in Service.Adjust.
	return m.AppendEntry(ctx, tenantID, domain.CreditLedgerEntry{
		CustomerID:  customerID,
		EntryType:   domain.CreditAdjustment,
		AmountCents: amountCents,
		Description: description,
		CreatedAt:   clock.Now(ctx),
	})
}

func (m *memStore) ApplyToInvoiceAtomic(ctx context.Context, tenantID, customerID, invoiceID, invoiceDesc string, invoiceAmountCents int64, _ time.Time) (int64, error) {
	if invoiceAmountCents <= 0 {
		return 0, nil
	}
	bal, err := m.GetBalance(ctx, tenantID, customerID)
	if err != nil {
		return 0, err
	}
	if bal.BalanceCents <= 0 {
		return 0, nil
	}
	deduct := min(bal.BalanceCents, invoiceAmountCents)
	if _, err := m.AppendEntry(ctx, tenantID, domain.CreditLedgerEntry{
		CustomerID:  customerID,
		EntryType:   domain.CreditUsage,
		AmountCents: -deduct,
		Description: invoiceDesc,
		InvoiceID:   invoiceID,
	}); err != nil {
		return 0, err
	}
	return deduct, nil
}

func (m *memStore) RetireCommitGrantForInvoiceTx(_ context.Context, _ *sql.Tx, tenantID, invoiceID string) (int64, error) {
	// Mirror the store contract: find the commit grant funded by the
	// invoice, retire its remaining balance, append the negative entry.
	for i := range m.entries {
		e := &m.entries[i]
		if e.SourceInvoiceID != invoiceID || e.GrantKind != domain.GrantKindCommit {
			continue
		}
		remaining := e.AmountCents - e.ConsumedCents
		if remaining <= 0 {
			return 0, nil
		}
		e.ConsumedCents = e.AmountCents
		m.entries = append(m.entries, domain.CreditLedgerEntry{
			CustomerID:  e.CustomerID,
			EntryType:   domain.CreditAdjustment,
			AmountCents: -remaining,
			Description: "Commit retired — funding invoice voided",
		})
		return remaining, nil
	}
	return 0, nil
}

func (m *memStore) SumCreditNoteGrantsForInvoice(_ context.Context, _, customerID, invoiceID string) (int64, error) {
	var sum int64
	for _, e := range m.entries {
		if e.CustomerID == customerID && e.InvoiceID == invoiceID && e.EntryType == domain.CreditGrant && e.SourceCreditNoteID != "" {
			sum += e.AmountCents
		}
	}
	return sum, nil
}

func (m *memStore) CountEntries(_ context.Context, filter ListFilter) (int, error) {
	n := 0
	for _, e := range m.entries {
		if filter.CustomerID != "" && e.CustomerID != filter.CustomerID {
			continue
		}
		if filter.EntryType != "" && string(e.EntryType) != filter.EntryType {
			continue
		}
		if filter.InvoiceID != "" && e.InvoiceID != filter.InvoiceID {
			continue
		}
		n++
	}
	return n, nil
}

func (m *memStore) ListEntries(_ context.Context, filter ListFilter) ([]domain.CreditLedgerEntry, error) {
	var result []domain.CreditLedgerEntry
	for _, e := range m.entries {
		if e.CustomerID != filter.CustomerID {
			continue
		}
		if filter.EntryType != "" && string(e.EntryType) != filter.EntryType {
			continue
		}
		result = append(result, e)
	}
	return result, nil
}

func TestGrant(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := context.Background()

	t.Run("valid grant", func(t *testing.T) {
		entry, err := svc.Grant(ctx, "t1", GrantInput{
			CustomerID:  "cus_1",
			AmountCents: 50000,
			Description: "$500 promotional credit",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if entry.EntryType != domain.CreditGrant {
			t.Errorf("type: got %q, want grant", entry.EntryType)
		}
		if entry.AmountCents != 50000 {
			t.Errorf("amount: got %d, want 50000", entry.AmountCents)
		}
		if entry.BalanceAfter != 50000 {
			t.Errorf("balance_after: got %d, want 50000", entry.BalanceAfter)
		}
	})

	t.Run("missing customer", func(t *testing.T) {
		_, err := svc.Grant(ctx, "t1", GrantInput{AmountCents: 100})
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("zero amount", func(t *testing.T) {
		_, err := svc.Grant(ctx, "t1", GrantInput{CustomerID: "c", AmountCents: 0})
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("past expires_at rejected", func(t *testing.T) {
		// Operator picked a date in the past — DOA grant. Industry
		// parity with Stripe's 422 on past expires_at. The customer
		// is NOT clock-pinned in this test, so clock.Now(ctx) returns
		// wall-clock; the past date should fail.
		past := time.Now().AddDate(0, 0, -1) // yesterday
		_, err := svc.Grant(ctx, "t1", GrantInput{
			CustomerID:  "cus_1",
			AmountCents: 10000,
			Description: "DOA grant",
			ExpiresAt:   &past,
		})
		if err == nil {
			t.Fatal("expected error on past expires_at")
		}
	})

	t.Run("expires_at == now rejected", func(t *testing.T) {
		// Strict inequality: expires_at == now is also DOA.
		nowT := time.Now()
		_, err := svc.Grant(ctx, "t1", GrantInput{
			CustomerID:  "cus_1",
			AmountCents: 10000,
			Description: "expires now",
			ExpiresAt:   &nowT,
		})
		if err == nil {
			t.Fatal("expected error on expires_at == now")
		}
	})

	t.Run("future expires_at accepted", func(t *testing.T) {
		future := time.Now().AddDate(0, 1, 0) // 1 month from now
		entry, err := svc.Grant(ctx, "t1", GrantInput{
			CustomerID:  "cus_1",
			AmountCents: 10000,
			Description: "future-expiring grant",
			ExpiresAt:   &future,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if entry.ExpiresAt == nil || !entry.ExpiresAt.Equal(future) {
			t.Errorf("expires_at: got %v, want %v", entry.ExpiresAt, future)
		}
	})

	t.Run("nil expires_at accepted (forever credit)", func(t *testing.T) {
		entry, err := svc.Grant(ctx, "t1", GrantInput{
			CustomerID:  "cus_1",
			AmountCents: 10000,
			Description: "forever credit",
			ExpiresAt:   nil,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if entry.ExpiresAt != nil {
			t.Errorf("expires_at should remain nil for forever credits, got %v", entry.ExpiresAt)
		}
	})
}

func TestApplyToInvoice(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := context.Background()

	// Grant $500
	_, _ = svc.Grant(ctx, "t1", GrantInput{CustomerID: "cus_1", AmountCents: 50000, Description: "Test grant"})

	t.Run("partial application", func(t *testing.T) {
		deducted, err := svc.ApplyToInvoice(ctx, "t1", "cus_1", "inv_1", 19900)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if deducted != 19900 {
			t.Errorf("deducted: got %d, want 19900", deducted)
		}

		bal, _ := svc.GetBalance(ctx, "t1", "cus_1")
		if bal.BalanceCents != 30100 {
			t.Errorf("remaining balance: got %d, want 30100", bal.BalanceCents)
		}
	})

	t.Run("exceeds balance", func(t *testing.T) {
		// Balance is now 30100, try to apply 50000
		deducted, err := svc.ApplyToInvoice(ctx, "t1", "cus_1", "inv_2", 50000)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if deducted != 30100 {
			t.Errorf("deducted: got %d, want 30100 (uses remaining balance)", deducted)
		}

		bal, _ := svc.GetBalance(ctx, "t1", "cus_1")
		if bal.BalanceCents != 0 {
			t.Errorf("remaining balance: got %d, want 0", bal.BalanceCents)
		}
	})

	t.Run("no balance left", func(t *testing.T) {
		deducted, err := svc.ApplyToInvoice(ctx, "t1", "cus_1", "inv_3", 10000)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if deducted != 0 {
			t.Errorf("deducted: got %d, want 0", deducted)
		}
	})
}

// TestProcessExpiry_SkipsFullyConsumed locks in the Orb credit-block
// model behavior: a grant whose consumed_cents already equals
// amount_cents is fully drained — expiry must be a no-op (no ledger
// row, no balance change), not a -amount_cents deduction that would
// drive balance arbitrarily negative.
//
// Pre-fix bug shape: ListExpiredGrants* returned grants regardless of
// consumed state, and processExpiry appended -g.AmountCents. A
// $50 grant fully consumed by a usage entry would then get expired
// for -$50, taking balance from $0 to -$50.
func TestProcessExpiry_SkipsFullyConsumed(t *testing.T) {
	store := newMemStore()
	svc := NewService(store)
	ctx := context.Background()

	// Seed the grant into the store fully consumed, then hand
	// processExpiry a STALE candidate snapshot claiming it still has
	// headroom — the shape a backdated apply landing between the
	// candidate list and the retirement produces. ExpireGrantAtomic
	// must re-read current state and no-op, never trust the snapshot.
	expiresAt := time.Date(2027, 7, 21, 18, 30, 0, 0, time.UTC)
	seeded, err := store.AppendEntry(ctx, "t1", domain.CreditLedgerEntry{
		CustomerID:  "cus_1",
		EntryType:   domain.CreditGrant,
		AmountCents: 5000,
		Description: "seed",
		ExpiresAt:   &expiresAt,
	})
	if err != nil {
		t.Fatalf("seed grant: %v", err)
	}
	store.entries[0].ConsumedCents = 5000 // fully drained by prior usage

	staleSnapshot := seeded
	staleSnapshot.ConsumedCents = 0
	grants := []domain.CreditLedgerEntry{staleSnapshot}

	expired, errs := svc.processExpiry(ctx, grants)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if expired != 0 {
		t.Errorf("expected 0 grants expired (already fully consumed), got %d", expired)
	}

	// Verify no expiry row was appended.
	entries, _ := svc.store.ListEntries(ctx, ListFilter{TenantID: "t1", CustomerID: "cus_1"})
	for _, e := range entries {
		if e.EntryType == domain.CreditExpiry {
			t.Errorf("unexpected expiry row appended for fully-consumed grant: amount_cents=%d", e.AmountCents)
		}
	}
}

// TestProcessExpiry_DeductRemaining covers the partial-consumption
// case: a $50 grant with $30 already consumed → expiry deducts $20
// (the un-drained portion), not the full $50.
func TestProcessExpiry_DeductRemaining(t *testing.T) {
	store := newMemStore()
	svc := NewService(store)
	ctx := context.Background()

	expiresAt := time.Date(2027, 7, 21, 18, 30, 0, 0, time.UTC)
	seeded, err := store.AppendEntry(ctx, "t1", domain.CreditLedgerEntry{
		CustomerID:  "cus_1",
		EntryType:   domain.CreditGrant,
		AmountCents: 5000,
		Description: "seed",
		ExpiresAt:   &expiresAt,
	})
	if err != nil {
		t.Fatalf("seed grant: %v", err)
	}
	store.entries[0].ConsumedCents = 3000 // partially drained
	seeded.ConsumedCents = 3000
	grants := []domain.CreditLedgerEntry{seeded}

	expired, errs := svc.processExpiry(ctx, grants)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if expired != 1 {
		t.Errorf("expected 1 grant expired, got %d", expired)
	}

	// Find the expiry row.
	entries, _ := store.ListEntries(ctx, ListFilter{TenantID: "t1", CustomerID: "cus_1"})
	var expiryRow *domain.CreditLedgerEntry
	for i := range entries {
		if entries[i].EntryType == domain.CreditExpiry {
			expiryRow = &entries[i]
			break
		}
	}
	if expiryRow == nil {
		t.Fatal("no expiry row was appended")
	}
	if expiryRow.AmountCents != -2000 {
		t.Errorf("expiry amount_cents: got %d, want -2000 (remaining = 5000 - 3000)", expiryRow.AmountCents)
	}
	if !expiryRow.CreatedAt.Equal(expiresAt) {
		t.Errorf("expiry created_at: got %v, want %v (grant's expires_at)", expiryRow.CreatedAt, expiresAt)
	}
}

func TestAdjust(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := context.Background()

	_, _ = svc.Grant(ctx, "t1", GrantInput{CustomerID: "cus_1", AmountCents: 10000, Description: "Test grant"})

	t.Run("positive adjustment", func(t *testing.T) {
		entry, err := svc.Adjust(ctx, "t1", AdjustInput{
			CustomerID: "cus_1", AmountCents: 5000, Description: "Goodwill credit",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if entry.BalanceAfter != 15000 {
			t.Errorf("balance: got %d, want 15000", entry.BalanceAfter)
		}
	})

	t.Run("negative adjustment", func(t *testing.T) {
		entry, err := svc.Adjust(ctx, "t1", AdjustInput{
			CustomerID: "cus_1", AmountCents: -3000, Description: "Correction",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if entry.BalanceAfter != 12000 {
			t.Errorf("balance: got %d, want 12000", entry.BalanceAfter)
		}
	})

	t.Run("missing description", func(t *testing.T) {
		_, err := svc.Adjust(ctx, "t1", AdjustInput{CustomerID: "c", AmountCents: 100})
		if err == nil {
			t.Fatal("expected error for missing description")
		}
	})

	// Adjust on a clock-pinned customer must bind effective-now from
	// the customer pin so the ledger entry's created_at lands in
	// simulated time, not wall-clock. Symptom of the missing bind
	// (caught 2026-05-24): a deduct row on a clock-pinned customer
	// appeared at wall-clock-now on the Credits tab, out-of-band with
	// the surrounding sim-time activity.
	t.Run("clock-pinned customer stamps ledger at sim time", func(t *testing.T) {
		svc := NewService(newMemStore())
		simNow := time.Date(2028, 6, 15, 12, 0, 0, 0, time.UTC)
		svc.SetResolver(&stubResolverForCustomer{pinned: map[string]time.Time{"cus_sim": simNow}})

		// Seed a grant first so the deduct has balance to pull from.
		// Grant already binds — leverage it to verify the resolver
		// wiring works end-to-end before exercising Adjust.
		if _, err := svc.Grant(context.Background(), "t1", GrantInput{
			CustomerID: "cus_sim", AmountCents: 10000, Description: "seed",
		}); err != nil {
			t.Fatalf("seed grant: %v", err)
		}

		entry, err := svc.Adjust(context.Background(), "t1", AdjustInput{
			CustomerID: "cus_sim", AmountCents: -3000, Description: "Deduct",
		})
		if err != nil {
			t.Fatalf("Adjust on clock-pinned customer: %v", err)
		}
		if !entry.CreatedAt.Equal(simNow) {
			t.Errorf("created_at: got %v want %v (sim time from clock pin)", entry.CreatedAt, simNow)
		}
	})
}

// stubResolverForCustomer is a minimal clock.Resolver that maps
// customer ids → a frozen simulated-now. Returns errs.ErrNotFound for
// unmapped customers so unbound rows fall back to wall-clock (the
// production behaviour for non-pinned customers).
type stubResolverForCustomer struct {
	pinned map[string]time.Time
}

func (s *stubResolverForCustomer) SimForCustomer(_ context.Context, _, customerID string) (clock.Sim, error) {
	if t, ok := s.pinned[customerID]; ok {
		// A pinned customer resolves BOTH halves — instant and clock.
		return clock.Sim{At: t, TestClockID: "tc_stub"}, nil
	}
	return clock.Sim{}, errs.ErrNotFound
}
func (s *stubResolverForCustomer) SimForSubscription(_ context.Context, _, _ string) (clock.Sim, error) {
	return clock.Sim{}, errs.ErrNotFound
}
func (s *stubResolverForCustomer) SimForInvoice(_ context.Context, _, _ string) (clock.Sim, error) {
	return clock.Sim{}, errs.ErrNotFound
}

func TestGetBalance(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := context.Background()

	_, _ = svc.Grant(ctx, "t1", GrantInput{CustomerID: "cus_1", AmountCents: 50000, Description: "Test grant"})
	_, _ = svc.ApplyToInvoice(ctx, "t1", "cus_1", "inv_1", 20000)

	bal, err := svc.GetBalance(ctx, "t1", "cus_1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bal.BalanceCents != 30000 {
		t.Errorf("balance: got %d, want 30000", bal.BalanceCents)
	}
	if bal.TotalGranted != 50000 {
		t.Errorf("granted: got %d, want 50000", bal.TotalGranted)
	}
	if bal.TotalUsed != 20000 {
		t.Errorf("used: got %d, want 20000", bal.TotalUsed)
	}
}
