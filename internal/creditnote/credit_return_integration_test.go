package creditnote_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// ADR-132 through the real Issue path: a credit note on a paid invoice never
// gives the customer less than it says, and a credit note's return waits for
// a concurrent draw on the same customer.

type cnReturnRig struct {
	ctx      context.Context
	db       *postgres.DB
	tenantID string
	cust     domain.Customer
	inv      domain.Invoice
	invStore *invoice.PostgresStore
	credits  *credit.Service
	notes    *creditnote.Service
}

func newCNReturnRig(t *testing.T, name string) *cnReturnRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, name)
	cust, inv := seedInvoiceForCN(t, db, tenantID, "cnreturn", domain.InvoiceFinalized, domain.PaymentPending, "")
	invStore := invoice.NewPostgresStore(db)
	credits := credit.NewService(credit.NewPostgresStore(db))
	settings := tenant.NewSettingsStore(db)
	notes := creditnote.NewService(creditnote.NewPostgresStore(db), invStore, nil, ledgerGranter{credits})
	notes.SetNumberGenerator(settings)
	return &cnReturnRig{ctx: ctx, db: db, tenantID: tenantID, cust: cust, inv: inv, invStore: invStore, credits: credits, notes: notes}
}

func (r *cnReturnRig) grant(t *testing.T, cents int64, expires *time.Time, kind domain.GrantKind) string {
	t.Helper()
	g, err := r.credits.Grant(r.ctx, r.tenantID, credit.GrantInput{
		Currency: "USD", CustomerID: r.cust.ID, AmountCents: cents, Description: "seed", ExpiresAt: expires, GrantKind: kind,
	})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	return g.ID
}

func (r *cnReturnRig) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(r.ctx, q, args...); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func (r *cnReturnRig) balance(t *testing.T) domain.CreditBalance {
	t.Helper()
	b, err := testutil.BalanceUSD(r.credits.GetBalance(r.ctx, r.tenantID, r.cust.ID))
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	return b
}

// checkTotals: the balance totals add up and agree with the grant list, and
// the balance equals the open capacity of the blocks.
func (r *cnReturnRig) checkTotals(t *testing.T) {
	t.Helper()
	b := r.balance(t)
	if got := b.TotalUsed + b.TotalExpired + b.TotalRetired + b.TotalDeducted + b.BalanceCents; got != b.TotalGranted {
		t.Errorf("used+expired+retired+deducted+balance = %d, granted %d (%+v)", got, b.TotalGranted, b)
	}
	resp, err := r.credits.ListGrants(r.ctx, r.tenantID, r.cust.ID, true)
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	var amount, used, expired, remaining int64
	for _, g := range resp.Grants {
		amount += g.AmountCents
		used += g.ConsumedCents - g.ExpiredCents - g.RetiredCents - g.DeductedCents
		expired += g.ExpiredCents
		remaining += g.RemainingCents
	}
	if amount != b.TotalGranted || used != b.TotalUsed || expired != b.TotalExpired || remaining != b.BalanceCents {
		t.Errorf("totals granted %d used %d expired %d balance %d; grant list %d %d %d remaining %d",
			b.TotalGranted, b.TotalUsed, b.TotalExpired, b.BalanceCents, amount, used, expired, remaining)
	}
}

func (r *cnReturnRig) payRest(t *testing.T) {
	t.Helper()
	if _, err := r.invStore.MarkPaid(r.ctx, r.tenantID, r.inv.ID, "pi_cnreturn", time.Now().UTC()); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	if inv, err := r.invStore.Get(r.ctx, r.tenantID, r.inv.ID); err != nil || inv.Status != domain.InvoicePaid {
		t.Fatalf("invoice after MarkPaid: %+v, %v; want paid", inv.Status, err)
	}
}

func (r *cnReturnRig) issueCredit(t *testing.T, cents int64) {
	t.Helper()
	cn, err := r.notes.Create(r.ctx, r.tenantID, creditnote.CreateInput{
		InvoiceID: r.inv.ID, Reason: "order_change", CreditAmountCents: cents,
		Lines: []creditnote.CreditLineInput{{Description: "credit", Quantity: 1, UnitAmountCents: cents}},
	})
	if err != nil {
		t.Fatalf("create credit note: %v", err)
	}
	if _, err := r.notes.Issue(r.ctx, r.tenantID, cn.ID); err != nil {
		t.Fatalf("issue credit note: %v", err)
	}
}

// Scenario A: an invoice of 10000 paid with 4000 of promotional credit and
// 6000 by card; a 6000 credit note. The note's documents say 6000 is added to
// the balance, and it is, whether the promotional grant is still live (its
// 2400 share goes back to it) or expired yesterday (all 6000 is new credit).
// Before the pro-rata rule the expired case restored 4000 to the expired
// grant, expired it at once, and added 2000.
//
// Mutation-verify (CI, PR #1498): treat every block as spendable → the
// expired grant gets 2400 of live credit back ("expired yesterday",
// promotional remaining red).
func TestIssue_PaidInvoiceCreditNeverLessThanTheNote(t *testing.T) {
	for _, tc := range []struct {
		name        string
		expire      bool
		wantPromo   int64 // the promotional grant's remaining after the note
		wantBalance int64
	}{
		{"promotional grant still live", false, 2400, 6000},
		{"promotional grant expired yesterday", true, 0, 6000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCNReturnRig(t, "CN Return Paid")
			later := time.Now().UTC().Add(time.Hour)
			promo := r.grant(t, 4000, &later, domain.GrantKindPromotional)
			if applied, err := r.credits.ApplyToInvoice(r.ctx, r.tenantID, r.cust.ID, r.inv.ID, 10000); err != nil || applied != 4000 {
				t.Fatalf("apply: %d, %v; want 4000", applied, err)
			}
			r.payRest(t)
			if tc.expire {
				r.exec(t, `UPDATE customer_credit_ledger SET expires_at = $1 WHERE id = $2`, time.Now().UTC().Add(-24*time.Hour), promo)
			}
			if b := r.balance(t); b.BalanceCents != 0 {
				t.Fatalf("balance before the note %d, want 0", b.BalanceCents)
			}

			r.issueCredit(t, 6000)

			if b := r.balance(t); b.BalanceCents != tc.wantBalance {
				t.Errorf("balance %d, want %d: exactly the note's credit amount", b.BalanceCents, tc.wantBalance)
			}
			resp, err := r.credits.ListGrants(r.ctx, r.tenantID, r.cust.ID, true)
			if err != nil {
				t.Fatalf("list grants: %v", err)
			}
			for _, g := range resp.Grants {
				if g.ID == promo && g.RemainingCents != tc.wantPromo {
					t.Errorf("promotional grant remaining %d, want %d", g.RemainingCents, tc.wantPromo)
				}
			}
			r.checkTotals(t)
		})
	}
}

// A credit note's issue and a draw on the same customer, both through their
// real paths, queued behind the customer's ledger lock and let go together.
// Whichever runs first, both succeed, nothing is lost or made, and the
// totals agree with the grant list.
//
// It is a collision test for the two real paths (no deadlock, no lost or made
// credit in either order). It does not isolate the return's own early lock:
// the ledger insert takes the same lock, so the issue queues either way.
// TestCreditReturn_ConcurrentWritersWaitForTheReturn (internal/credit)
// isolates it.
func TestIssue_CreditReturnRacesADraw(t *testing.T) {
	r := newCNReturnRig(t, "CN Return Race")
	admin := testutil.AdminPool(t)
	r.grant(t, 8000, nil, "")
	if applied, err := r.credits.ApplyToInvoice(r.ctx, r.tenantID, r.cust.ID, r.inv.ID, 10000); err != nil || applied != 8000 {
		t.Fatalf("apply: %d, %v; want 8000", applied, err)
	}
	// 2000 more credit for the race's draw. The invoice stays unpaid: 2000
	// due, 8000 of credit applied.
	r.grant(t, 2000, nil, "")
	other, err := r.invStore.Create(r.ctx, r.tenantID, domain.Invoice{
		CustomerID: r.cust.ID, InvoiceNumber: "INV-CNRETURN-OTHER", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentPending, Currency: "USD",
		SubtotalCents: 3000, TotalAmountCents: 3000, AmountDueCents: 3000,
		BillingPeriodStart: time.Now().UTC().Add(-30 * 24 * time.Hour), BillingPeriodEnd: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create other invoice: %v", err)
	}
	cn, err := r.notes.Create(r.ctx, r.tenantID, creditnote.CreateInput{
		InvoiceID: r.inv.ID, Reason: "order_change",
		Lines: []creditnote.CreditLineInput{{Description: "credit", Quantity: 1, UnitAmountCents: 2500}},
	})
	if err != nil {
		t.Fatalf("create racing note: %v", err)
	}
	// Unpaid: the note relieves the 2000 due and returns 500 of the applied
	// credit to the grant it came from.
	before := r.balance(t).BalanceCents // 2000

	// Hold the customer's ledger lock, start both writers, wait until both
	// queue on it, then let go.
	hold, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		t.Fatalf("begin hold: %v", err)
	}
	if _, err := hold.ExecContext(r.ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, r.tenantID+":"+r.cust.ID); err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	issueErr := make(chan error, 1)
	drained := make(chan int64, 1)
	drainErr := make(chan error, 1)
	go func() {
		_, err := r.notes.Issue(r.ctx, r.tenantID, cn.ID)
		issueErr <- err
	}()
	go func() {
		n, err := credit.NewPostgresStore(r.db).ApplyToInvoiceAtomic(r.ctx, r.tenantID, r.cust.ID, other.ID, "race draw", 3000, time.Time{})
		drained <- n
		drainErr <- err
	}()
	testutil.WaitForLockWaiters(t, r.ctx, admin, "pg_advisory_xact_lock", 2)
	postgres.Rollback(hold)

	if err := <-issueErr; err != nil {
		t.Fatalf("issue: %v", err)
	}
	n := <-drained
	if err := <-drainErr; err != nil {
		t.Fatalf("draw: %v", err)
	}
	// The note returns 500; the draw takes what the balance had when it ran
	// (2000 before the note, 2500 after).
	if n != 2000 && n != 2500 {
		t.Errorf("draw took %d, want 2000 or 2500", n)
	}
	if got := r.balance(t).BalanceCents; got != before+500-n {
		t.Errorf("balance %d, want %d (before %d + 500 returned − %d drawn)", got, before+500-n, before, n)
	}
	r.checkTotals(t)
}
