package credit_test

import (
	"errors"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// A ledger entry stores the balance after it. Balances this large (near
// 9.2 × 10^18 cents) are not realistic; the sum is checked against int64 so
// an entry that would take the balance past it is refused with nothing
// written, never stored with a wrapped balance_after.
func TestAppendEntry_BalancePastInt64Refused(t *testing.T) {
	r := newCcyRig(t, "Balance Past Int64", false)
	cust := r.customer("cus_big_balance")
	const big = 5_000_000_000_000_000_000
	first := r.block(cust, "USD", big)
	if first.BalanceAfter != big {
		t.Fatalf("first balance_after = %d, want %d", first.BalanceAfter, big)
	}
	_, err := r.store.AppendEntry(r.ctx, r.tenantID, domain.CreditLedgerEntry{
		CustomerID: cust, EntryType: domain.CreditGrant, Currency: "USD",
		AmountCents: big, Description: "second block",
	})
	if !errors.Is(err, domain.ErrAmountOverflow) {
		t.Fatalf("second block err = %v, want ErrAmountOverflow", err)
	}
	var rows int
	var stored int64
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if err := tx.QueryRowContext(r.ctx,
		`SELECT COUNT(*), COALESCE(MIN(balance_after), 0) FROM customer_credit_ledger WHERE tenant_id = $1 AND customer_id = $2`,
		r.tenantID, cust).Scan(&rows, &stored); err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if rows != 1 || stored != big {
		t.Errorf("ledger has %d rows, lowest balance_after %d; want 1 row at %d", rows, stored, big)
	}

	// A deduction that keeps the balance inside int64 is written as before.
	got, err := r.store.AppendEntry(r.ctx, r.tenantID, domain.CreditLedgerEntry{
		CustomerID: cust, EntryType: domain.CreditAdjustment, Currency: "USD",
		AmountCents: -1, Description: "deduct",
	})
	if err != nil || got.BalanceAfter != big-1 {
		t.Errorf("deduction: balance_after %d, err %v; want %d", got.BalanceAfter, err, big-1)
	}
}
