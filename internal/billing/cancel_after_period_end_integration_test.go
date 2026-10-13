package billing_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/testclock"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// Issue #991: an immediate cancel whose canceled_at was at or after
// current_billing_period_end wrote no final invoice, and the cycle close then
// skipped the subscription because it was not active. The ended period was
// never invoiced.
//
// The fix has two parts, and the tests below name which one they pin:
//   - the pre-step: Service.Cancel closes every ended period through the
//     billing run's own attempt (BillDueNow) before it cancels;
//   - the refusal: the cancel's final-invoice step answers unbilled_period
//     and rolls the cancel back when a period is still ended and unbilled.
//
// Fixture (newThresholdFixture + a base fee + a true one-month period):
// monthly plan, 5000c in_arrears base, 1c/call usage; period [P0, P0+1 month);
// 100 usage events at P0+i minutes × qty 10 = 1000c. Time is bound per call with clock.WithEffectiveNow, as in
// period_closer_integration_test.go. The cancel goes through the real
// subscription.Service.Cancel with the real engine wired as its biller and
// its due biller, as internal/api/router.go wires them.

const cancelFixtureBaseCents = 5000

type cancelRig struct {
	f      *thresholdFixture
	base   context.Context
	credit *credit.Service
}

func newCancelRig(t *testing.T, name string) *cancelRig {
	t.Helper()
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, name)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 120*time.Second)
	t.Cleanup(cancel)
	r := &cancelRig{f: f, base: ctx, credit: credit.NewService(credit.NewPostgresStore(f.db))}

	// The shared fixture's plan has no base fee; give it an in_arrears one.
	r.exec(t, `UPDATE plans SET base_amount_cents = $1, base_bill_timing = 'in_arrears' WHERE id = $2`, cancelFixtureBaseCents, f.planID)
	// The shared fixture's period is 30 days; a monthly plan's base prorates
	// over the calendar month, so make the period exactly one month.
	f.cycleEnd = f.cycleStart.AddDate(0, 1, 0)
	r.exec(t, `UPDATE subscriptions SET current_billing_period_end = $1, next_billing_at = $1 WHERE id = $2`, f.cycleEnd, f.subID)

	f.ingestUsage(t, ctx, 100, 10)

	// The cancel's collaborators, as internal/api/router.go wires them.
	creditNoteSvc := creditnote.NewService(creditnote.NewPostgresStore(f.db), f.invStore, nil, &cnGrantAdapter{r.credit})
	creditNoteSvc.SetNumberGenerator(f.settings)
	f.engine.SetCreditGranter(r.credit)
	f.engine.SetInvoiceVoider(invoice.NewService(f.invStore, clock.Real(), f.settings))
	f.engine.SetCreditNoteAdjuster(creditNoteSvc)
	f.engine.SetCreditHeadroomReader(creditNoteSvc)
	f.engine.SetScheduledCancelExecutor(f.subSvc)
	f.subSvc.SetBiller(f.engine)
	f.subSvc.SetDueBiller(f.engine)
	return r
}

// exec runs one statement that must touch exactly one row.
func (r *cancelRig) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	tx, err := r.f.db.BeginTx(r.base, postgres.TxTenant, r.f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	res, err := tx.ExecContext(r.base, query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("%s touched %d rows, want 1", query, n)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func (r *cancelRig) sub(t *testing.T) domain.Subscription {
	t.Helper()
	sub, err := r.f.subStore.Get(r.base, r.f.tenantID, r.f.subID)
	if err != nil {
		t.Fatalf("get subscription: %v", err)
	}
	return sub
}

// ingestAt adds count events of qty each, one a minute from start.
func (r *cancelRig) ingestAt(t *testing.T, start time.Time, count int, qty int64) {
	t.Helper()
	ctx := r.at(start.Add(time.Duration(count+1) * time.Minute))
	for i := 0; i < count; i++ {
		ts := start.Add(time.Duration(i) * time.Minute)
		if _, err := r.f.usageSvc.Ingest(ctx, r.f.tenantID, usage.IngestInput{
			CustomerID: r.f.customerID, MeterID: r.f.meterID, Quantity: decimal.NewFromInt(qty), Timestamp: &ts,
		}); err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
	}
}

// runWritesNothing is the billing run after a cancel: it must bill nothing.
func (r *cancelRig) runWritesNothing(t *testing.T, at time.Time) {
	t.Helper()
	before := len(r.f.listInvoices(t, r.base))
	if gen, failures := r.f.engine.RunCycleForTenant(r.at(at), r.f.tenantID, 50); gen != 0 || len(failures) != 0 {
		t.Errorf("billing run after the cancel: generated=%d failures=%v, want 0 and none", gen, failures)
	}
	if after := len(r.f.listInvoices(t, r.base)); after != before {
		t.Errorf("billing run after the cancel wrote %d invoice(s), want 0", after-before)
	}
}

// invoiceFor returns the one live invoice headed [start, end).
func invoiceFor(t *testing.T, invs []domain.Invoice, start, end time.Time) domain.Invoice {
	t.Helper()
	var found []domain.Invoice
	for _, inv := range invs {
		if inv.BillingPeriodStart.Equal(start) && inv.BillingPeriodEnd.Equal(end) {
			found = append(found, inv)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d live invoices headed [%v, %v), want exactly 1 (of %d live)", len(found), start, end, len(invs))
	}
	return found[0]
}

func wantUnbilledPeriod(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("cancel succeeded with an ended period still unbilled; want it refused with unbilled_period")
	}
	if !errors.Is(err, errs.ErrInvalidState) || errs.Code(err) != "unbilled_period" {
		t.Fatalf("cancel error = %v (code %q), want an invalid-state error with code unbilled_period", err, errs.Code(err))
	}
}

func (r *cancelRig) at(t time.Time) context.Context { return clock.WithEffectiveNow(r.base, t) }

// liveInvoices returns the tenant's non-voided invoices with their lines.
func (r *cancelRig) liveInvoices(t *testing.T) ([]domain.Invoice, map[string][]domain.InvoiceLineItem) {
	t.Helper()
	var out []domain.Invoice
	lines := map[string][]domain.InvoiceLineItem{}
	for _, inv := range r.f.listInvoices(t, r.base) {
		if inv.Status == domain.InvoiceVoided {
			continue
		}
		li, err := r.f.invStore.ListLineItems(r.base, r.f.tenantID, inv.ID)
		if err != nil {
			t.Fatalf("list lines %s: %v", inv.ID, err)
		}
		out = append(out, inv)
		lines[inv.ID] = li
	}
	return out, lines
}

func sumLines(lines []domain.InvoiceLineItem) (usage, other int64) {
	for _, li := range lines {
		if li.LineType == domain.LineTypeUsage {
			usage += li.AmountCents
		} else {
			other += li.AmountCents
		}
	}
	return usage, other
}

// TestImmediateCancel_AfterPeriodEnd_EndedPeriodIsInvoiced: the period ended
// an hour ago and the billing run has not invoiced it yet. The operator
// cancels immediately; then the billing run goes. The ended period [P0, P1)
// must be invoiced exactly once: 1000c usage + the 5000c in_arrears base. The
// hour past the boundary rounds to zero days and has no usage, so the cancel
// itself writes no second invoice.
//
// Mutation seam: remove the BillDueNow pre-step from Service.Cancel → the
// cancel answers unbilled_period. Also turn the refusal back into a silent
// return → the cancel succeeds and no invoice exists.
func TestImmediateCancel_AfterPeriodEnd_EndedPeriodIsInvoiced(t *testing.T) {
	r := newCancelRig(t, "991 cancel after period end")
	cancelAt := r.f.cycleEnd.Add(time.Hour)

	canceled, _, err := r.f.subSvc.Cancel(r.at(cancelAt), r.f.tenantID, r.f.subID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if canceled.Status != domain.SubscriptionCanceled || canceled.CanceledAt == nil || !canceled.CanceledAt.Equal(cancelAt) {
		t.Fatalf("cancel: status=%s canceled_at=%v, want canceled at %v", canceled.Status, canceled.CanceledAt, cancelAt)
	}
	// The cancel closed [P0, P1) first, so it landed inside the next period.
	if canceled.CurrentBillingPeriodStart == nil || !canceled.CurrentBillingPeriodStart.Equal(r.f.cycleEnd) ||
		canceled.CurrentBillingPeriodEnd == nil || !canceled.CanceledAt.Before(*canceled.CurrentBillingPeriodEnd) {
		t.Errorf("canceled row's period = [%v, %v), want it to start at %v and end after canceled_at %v",
			canceled.CurrentBillingPeriodStart, canceled.CurrentBillingPeriodEnd, r.f.cycleEnd, cancelAt)
	}

	gen, failures := r.f.engine.RunCycleForTenant(r.at(cancelAt.Add(time.Hour)), r.f.tenantID, 50)
	if len(failures) != 0 {
		t.Fatalf("billing run: %v", failures)
	}
	t.Logf("billing run after the cancel generated %d invoice(s)", gen)

	invs, lines := r.liveInvoices(t)
	if len(invs) != 1 {
		t.Fatalf("live invoices = %d, want exactly 1 for the ended period [%v, %v) — 1000c usage + %dc in_arrears base were never invoiced",
			len(invs), r.f.cycleStart, r.f.cycleEnd, cancelFixtureBaseCents)
	}
	inv := invs[0]
	if !inv.BillingPeriodStart.Equal(r.f.cycleStart) || !inv.BillingPeriodEnd.Equal(r.f.cycleEnd) {
		t.Errorf("invoice period [%v, %v), want the whole ended period [%v, %v)", inv.BillingPeriodStart, inv.BillingPeriodEnd, r.f.cycleStart, r.f.cycleEnd)
	}
	usage, base := sumLines(lines[inv.ID])
	if usage != 1000 {
		t.Errorf("usage billed = %dc, want 1000", usage)
	}
	if base != cancelFixtureBaseCents {
		t.Errorf("base billed = %dc, want the full %dc (the period ran to its end)", base, cancelFixtureBaseCents)
	}
	if inv.SubtotalCents != 1000+cancelFixtureBaseCents {
		t.Errorf("subtotal = %dc, want %d", inv.SubtotalCents, 1000+cancelFixtureBaseCents)
	}
	assertUsageBilledOnce(t, r.base)

	// A second run must not bill it again.
	if gen, failures := r.f.engine.RunCycleForTenant(r.at(cancelAt.Add(2*time.Hour)), r.f.tenantID, 50); gen != 0 || len(failures) != 0 {
		t.Errorf("second billing run: generated=%d failures=%v, want 0 and none", gen, failures)
	}
	if again, _ := r.liveInvoices(t); len(again) != 1 {
		t.Errorf("live invoices after a second run = %d, want 1", len(again))
	}
}

// TestImmediateCancel_MidPeriod_FinalInvoice_Control: the same setup cancelled
// 15 days into the one-month period. The cancel writes the final invoice for
// [P0, tc): 1000c usage + 15 days' share of the base (proration is by whole
// days). Nothing is due, so the pre-step does not run and nothing changes.
func TestImmediateCancel_MidPeriod_FinalInvoice_Control(t *testing.T) {
	r := newCancelRig(t, "991 cancel mid period control")
	cancelAt := r.f.cycleStart.Add(15 * 24 * time.Hour)

	if _, _, err := r.f.subSvc.Cancel(r.at(cancelAt), r.f.tenantID, r.f.subID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if gen, failures := r.f.engine.RunCycleForTenant(r.at(r.f.cycleEnd.Add(time.Hour)), r.f.tenantID, 50); gen != 0 || len(failures) != 0 {
		t.Fatalf("billing run after a mid-period cancel: generated=%d failures=%v, want 0 and none", gen, failures)
	}

	invs, lines := r.liveInvoices(t)
	if len(invs) != 1 {
		t.Fatalf("live invoices = %d, want exactly the final-on-cancel invoice", len(invs))
	}
	inv := invs[0]
	if inv.BillingReason != domain.BillingReasonSubscriptionCancel {
		t.Errorf("billing reason = %s, want %s", inv.BillingReason, domain.BillingReasonSubscriptionCancel)
	}
	if !inv.BillingPeriodStart.Equal(r.f.cycleStart) || !inv.BillingPeriodEnd.Equal(cancelAt) {
		t.Errorf("invoice period [%v, %v), want [%v, %v)", inv.BillingPeriodStart, inv.BillingPeriodEnd, r.f.cycleStart, cancelAt)
	}
	usage, base := sumLines(lines[inv.ID])
	if usage != 1000 {
		t.Errorf("usage billed = %dc, want 1000", usage)
	}
	periodDays := int64(r.f.cycleEnd.Sub(r.f.cycleStart).Hours() / 24)
	if want := cancelFixtureBaseCents * 15 / periodDays; base < want || base > want+1 {
		t.Errorf("prorated base = %dc, want %d or %d (15 of %d days)", base, want, want+1, periodDays)
	}
	assertUsageBilledOnce(t, r.base)
}

// TestImmediateCancel_TwoPeriodsBehind_EachPeriodInvoicedOnce: the billing run
// is two periods behind when the operator cancels, ten days into the third.
// The pre-step closes [P0, P1) and [P1, P2) as two cycle invoices, each with
// its own usage and a full base; the cancel then bills [P2, tc) itself.
func TestImmediateCancel_TwoPeriodsBehind_EachPeriodInvoicedOnce(t *testing.T) {
	r := newCancelRig(t, "991 cancel two periods behind")
	p0, p1 := r.f.cycleStart, r.f.cycleEnd
	// The engine computes P2 from P1; a month and ten days past P1 is inside
	// the third period whichever way the month clamps.
	cancelAt := p1.AddDate(0, 1, 10)
	r.ingestAt(t, p1.Add(time.Hour), 50, 4)           // 200c in [P1, P2)
	r.ingestAt(t, cancelAt.Add(-48*time.Hour), 30, 1) // 30c in [P2, tc)
	canceled, _, err := r.f.subSvc.Cancel(r.at(cancelAt), r.f.tenantID, r.f.subID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if canceled.Status != domain.SubscriptionCanceled || canceled.CanceledAt == nil || !canceled.CanceledAt.Equal(cancelAt) {
		t.Fatalf("cancel: status=%s canceled_at=%v, want canceled at %v", canceled.Status, canceled.CanceledAt, cancelAt)
	}
	p2, p3 := *canceled.CurrentBillingPeriodStart, *canceled.CurrentBillingPeriodEnd
	if !p2.After(p1) || !p2.Before(cancelAt) || !p3.After(cancelAt) {
		t.Fatalf("canceled row's period = [%v, %v), want the third period around %v", p2, p3, cancelAt)
	}
	r.runWritesNothing(t, cancelAt.Add(time.Hour))

	invs, lines := r.liveInvoices(t)
	if len(invs) != 3 {
		t.Fatalf("live invoices = %d, want 3: two ended periods and the final partial one", len(invs))
	}
	for _, c := range []struct {
		name       string
		start, end time.Time
		usage      int64
	}{{"[P0, P1)", p0, p1, 1000}, {"[P1, P2)", p1, p2, 200}} {
		inv := invoiceFor(t, invs, c.start, c.end)
		usage, base := sumLines(lines[inv.ID])
		if inv.BillingReason != domain.BillingReasonSubscriptionCycle || usage != c.usage || base != cancelFixtureBaseCents ||
			inv.SubtotalCents != c.usage+cancelFixtureBaseCents {
			t.Errorf("%s: reason=%s usage=%dc base=%dc subtotal=%dc, want a cycle invoice of %dc usage + the full %dc base",
				c.name, inv.BillingReason, usage, base, inv.SubtotalCents, c.usage, cancelFixtureBaseCents)
		}
		// Each close is stamped at its own boundary, not at the cancel.
		if !inv.IssuedAtOrZero().Equal(c.end) {
			t.Errorf("%s: issued at %v, want its boundary %v", c.name, inv.IssuedAtOrZero(), c.end)
		}
	}
	final := invoiceFor(t, invs, p2, cancelAt)
	usage, base := sumLines(lines[final.ID])
	days := int64(cancelAt.Sub(p2).Hours()/24 + 0.5)
	periodDays := int64(p3.Sub(p2).Hours() / 24)
	if want := cancelFixtureBaseCents * days / periodDays; final.BillingReason != domain.BillingReasonSubscriptionCancel ||
		usage != 30 || base < want || base > want+1 || final.SubtotalCents != usage+base {
		t.Errorf("final invoice: reason=%s usage=%dc base=%dc subtotal=%dc, want a cancel invoice of 30c usage + %d or %dc base (%d of %d days)",
			final.BillingReason, usage, base, final.SubtotalCents, want, want+1, days, periodDays)
	}
	assertUsageBilledOnce(t, r.base)
}

// payingBiller is the due biller whose collection succeeds: after the real
// attempt it marks every unpaid invoice paid, as an inline auto-charge would.
type payingBiller struct {
	r    *cancelRig
	t    *testing.T
	at   time.Time
	next subscription.DueBiller
}

func (b payingBiller) BillDueNow(ctx context.Context, tenantID, id string) (int, error) {
	n, err := b.next.BillDueNow(ctx, tenantID, id)
	for _, inv := range b.r.f.listInvoices(b.t, b.r.base) {
		if inv.PaymentStatus != domain.PaymentSucceeded && inv.Status != domain.InvoiceVoided {
			if _, perr := b.r.f.invStore.MarkPaid(b.r.base, tenantID, inv.ID, "pi_"+inv.ID, b.at); perr != nil {
				b.t.Errorf("mark %s paid: %v", inv.ID, perr)
			}
		}
	}
	return n, err
}

// TestImmediateCancel_InAdvance_AfterPeriodEnd_KeepsEndedPeriodUsage: on a
// plan whose base is billed in advance, the pre-step's close writes ONE
// invoice headed [P1, P2) that carries the ended period's 1000c usage and the
// next period's 5000c base. The cancel an hour into [P1, P2) owes that base
// back and nothing else. Paid or unpaid, the usage must stay billed.
//
// Unpaid is the row the relief used to get wrong: it voided the whole invoice
// as a "fully unused prebill", usage lines included.
//
// Mutation seam: drop the onlyPrepaidBase check in reliefTx → the unpaid row
// has no live invoice carrying the usage.
func TestImmediateCancel_InAdvance_AfterPeriodEnd_KeepsEndedPeriodUsage(t *testing.T) {
	for _, paid := range []bool{false, true} {
		name := "renewal invoice unpaid"
		if paid {
			name = "renewal invoice paid"
		}
		t.Run(name, func(t *testing.T) {
			r := newCancelRig(t, "991 in advance "+name)
			p0, p1 := r.f.cycleStart, r.f.cycleEnd
			cancelAt := p1.Add(time.Hour)
			r.exec(t, `UPDATE plans SET base_bill_timing = 'in_advance' WHERE id = $1`, r.f.planID)
			// The day-1 invoice that prepaid [P0, P1), paid.
			day1, err := r.f.engine.BillOnCreate(r.at(p0), r.sub(t))
			if err != nil || day1.SubtotalCents != cancelFixtureBaseCents {
				t.Fatalf("day-1 invoice: %v subtotal=%dc, want %dc", err, day1.SubtotalCents, cancelFixtureBaseCents)
			}
			if _, err := r.f.invStore.MarkPaid(r.base, r.f.tenantID, day1.ID, "pi_day1", p0); err != nil {
				t.Fatalf("pay day-1 invoice: %v", err)
			}
			if paid {
				r.f.subSvc.SetDueBiller(payingBiller{r: r, t: t, at: p1, next: r.f.engine})
			}

			canceled, credited, err := r.f.subSvc.Cancel(r.at(cancelAt), r.f.tenantID, r.f.subID)
			if err != nil {
				t.Fatalf("cancel: %v", err)
			}
			if canceled.Status != domain.SubscriptionCanceled || !canceled.CanceledAt.Equal(cancelAt) {
				t.Fatalf("cancel: status=%s canceled_at=%v, want canceled at %v", canceled.Status, canceled.CanceledAt, cancelAt)
			}
			p2 := *canceled.CurrentBillingPeriodEnd
			r.runWritesNothing(t, cancelAt.Add(time.Hour))

			invs, lines := r.liveInvoices(t)
			if len(invs) != 2 {
				t.Fatalf("live invoices = %d, want 2: the day-1 invoice and the renewal that carries the ended period's usage", len(invs))
			}
			renewal := invoiceFor(t, invs, p1, p2)
			usage, base := sumLines(lines[renewal.ID])
			if usage != 1000 || base != cancelFixtureBaseCents || renewal.TotalAmountCents != 1000+cancelFixtureBaseCents {
				t.Fatalf("renewal invoice: usage=%dc base=%dc total=%dc, want 1000 + %d", usage, base, renewal.TotalAmountCents, cancelFixtureBaseCents)
			}
			bal, err := r.credit.GetBalance(r.base, r.f.tenantID, r.f.customerID)
			if err != nil {
				t.Fatalf("credit balance: %v", err)
			}
			if paid {
				// The whole unused base comes back as balance; the usage stays paid.
				if credited != cancelFixtureBaseCents || bal.BalanceCents != cancelFixtureBaseCents || renewal.AmountPaidCents != 1000+cancelFixtureBaseCents {
					t.Errorf("paid: credited=%dc balance=%dc amount_paid=%dc, want %d, %d and %d",
						credited, bal.BalanceCents, renewal.AmountPaidCents, cancelFixtureBaseCents, cancelFixtureBaseCents, 1000+cancelFixtureBaseCents)
				}
			} else {
				// The unused base comes off what is owed; the usage stays owed.
				if credited != 0 || bal.BalanceCents != 0 || renewal.AmountDueCents != 1000 || renewal.AmountPaidCents != 0 {
					t.Errorf("unpaid: credited=%dc balance=%dc amount_due=%dc amount_paid=%dc, want 0, 0, 1000 and 0",
						credited, bal.BalanceCents, renewal.AmountDueCents, renewal.AmountPaidCents)
				}
			}
			// The day-1 invoice paid for a period that ran to its end.
			if d := invoiceFor(t, invs, p0, p1); d.ID != day1.ID || d.AmountPaidCents != cancelFixtureBaseCents {
				t.Errorf("day-1 invoice: id=%s amount_paid=%dc, want %s untouched at %dc", d.ID, d.AmountPaidCents, day1.ID, cancelFixtureBaseCents)
			}
			assertUsageBilledOnce(t, r.base)
		})
	}
}

// cancelHTTP calls POST /{id}/cancel on the real handler at the given instant.
func (r *cancelRig) cancelHTTP(t *testing.T, at time.Time) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/"+r.f.subID+"/cancel", nil)
	req = req.WithContext(auth.WithKeyID(auth.WithTenantID(r.at(at), r.f.tenantID), "vlx_key_cancel_991"))
	rec := httptest.NewRecorder()
	subscription.NewHandler(r.f.subSvc).Routes().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestImmediateCancel_BillingWaitingOrParked_Refused: the ended period could
// not be billed and the subscription carries a billing-failure record
// (ADR-119), waiting for its next attempt or parked. The pre-step attempts
// nothing for it, so the refusal is all that stands between the cancel and a
// lost period: 409 unbilled_period, the subscription stays active, the record
// stays. Once billing is fixed, retry-billing then cancel loses nothing.
//
// Mutation seam: turn the refusal in billFinalOnImmediateCancelImpl back into
// a silent return → the cancel succeeds with no invoice.
func TestImmediateCancel_BillingWaitingOrParked_Refused(t *testing.T) {
	for _, parked := range []bool{false, true} {
		name := "waiting"
		if parked {
			name = "parked"
		}
		t.Run(name, func(t *testing.T) {
			r := newCancelRig(t, "991 cancel while billing "+name)
			p0, p1 := r.f.cycleStart, r.f.cycleEnd
			r.f.invAdapter.createTxErr = errors.New("invoice insert refused (test)")
			now := p1.Add(time.Minute)
			attempts := 1
			if parked {
				attempts = domain.MaxBillingAttempts
			}
			for n := 1; n <= attempts; n++ {
				if gen, failures := r.f.engine.RunCycleForTenant(r.at(now), r.f.tenantID, 50); gen != 0 || len(failures) != 1 {
					t.Fatalf("setup: failing run %d: generated=%d failures=%v, want 0 and 1", n, gen, failures)
				}
				if n < attempts {
					now = now.Add(billLadder[n-1])
				}
			}
			bf := r.sub(t).BillingFailure
			if bf == nil || bf.Attempts != attempts || bf.Parked != parked {
				t.Fatalf("setup: billing failure = %+v, want attempts %d parked %v", bf, attempts, parked)
			}

			// Inside the wait (or parked): refused, over HTTP and from the service.
			now = now.Add(time.Minute)
			code, body := r.cancelHTTP(t, now)
			if code != http.StatusConflict || !strings.Contains(body, `"unbilled_period"`) ||
				!strings.Contains(body, "retry billing for this subscription, then cancel again") {
				t.Fatalf("POST cancel: %d %s, want 409 unbilled_period telling the operator to retry billing", code, body)
			}
			_, _, err := r.f.subSvc.Cancel(r.at(now), r.f.tenantID, r.f.subID)
			wantUnbilledPeriod(t, err)
			after := r.sub(t)
			if after.Status != domain.SubscriptionActive || after.CanceledAt != nil ||
				!after.CurrentBillingPeriodStart.Equal(p0) || !after.CurrentBillingPeriodEnd.Equal(p1) {
				t.Fatalf("after the refusal: status=%s canceled_at=%v period=[%v, %v), want active and unchanged",
					after.Status, after.CanceledAt, after.CurrentBillingPeriodStart, after.CurrentBillingPeriodEnd)
			}
			if after.BillingFailure == nil || after.BillingFailure.Attempts != attempts || after.BillingFailure.Parked != parked {
				t.Fatalf("after the refusal: billing failure = %+v, want it unchanged at attempts %d parked %v", after.BillingFailure, attempts, parked)
			}
			if n := len(r.f.listInvoices(t, r.base)); n != 0 {
				t.Fatalf("%d invoices after a refused cancel, want 0", n)
			}

			if !parked {
				// The wait is over and billing still fails: the pre-step is a real,
				// counted attempt, and the cancel is still refused.
				now = now.Add(billLadder[0])
				_, _, err := r.f.subSvc.Cancel(r.at(now), r.f.tenantID, r.f.subID)
				wantUnbilledPeriod(t, err)
				if bf := r.sub(t).BillingFailure; bf == nil || bf.Attempts != 2 || r.sub(t).Status != domain.SubscriptionActive {
					t.Fatalf("after a cancel whose own bill failed: billing failure = %+v status=%s, want attempts 2 and active", bf, r.sub(t).Status)
				}
			}

			// Billing is fixed. Retry billing, then cancel: the period is billed once.
			r.f.invAdapter.createTxErr = nil
			now = now.Add(time.Minute)
			res, err := r.f.subSvc.RetryBilling(r.at(now), r.f.tenantID, r.f.subID)
			if err != nil || res.InvoicesGenerated != 1 {
				t.Fatalf("retry billing: %v, generated=%d, want 1", err, res.InvoicesGenerated)
			}
			canceled, _, err := r.f.subSvc.Cancel(r.at(now), r.f.tenantID, r.f.subID)
			if err != nil || canceled.Status != domain.SubscriptionCanceled || !canceled.CanceledAt.Equal(now) {
				t.Fatalf("cancel after the retry: %v status=%s canceled_at=%v, want canceled at %v", err, canceled.Status, canceled.CanceledAt, now)
			}
			r.runWritesNothing(t, now.Add(time.Hour))

			invs, lines := r.liveInvoices(t)
			ended := invoiceFor(t, invs, p0, p1)
			usage, base := sumLines(lines[ended.ID])
			if usage != 1000 || base != cancelFixtureBaseCents || ended.SubtotalCents != 1000+cancelFixtureBaseCents {
				t.Errorf("ended period: usage=%dc base=%dc subtotal=%dc, want 1000 + %d", usage, base, ended.SubtotalCents, cancelFixtureBaseCents)
			}
			// What ran of the next period before the cancel is the cancel's own
			// invoice: whole days of base, no usage. Under half a day, nothing.
			days := int64(now.Sub(p1).Hours()/24 + 0.5)
			wantInvoices := 1
			if days > 0 {
				wantInvoices = 2
				final := invoiceFor(t, invs, p1, now)
				usage, base := sumLines(lines[final.ID])
				periodDays := int64(canceled.CurrentBillingPeriodEnd.Sub(p1).Hours() / 24)
				if want := cancelFixtureBaseCents * days / periodDays; usage != 0 || base < want || base > want+1 {
					t.Errorf("final invoice: usage=%dc base=%dc, want 0 and %d or %dc (%d of %d days)", usage, base, want, want+1, days, periodDays)
				}
			}
			if len(invs) != wantInvoices {
				t.Errorf("live invoices = %d, want %d", len(invs), wantInvoices)
			}
			assertUsageBilledOnce(t, r.base)
		})
	}
}

// TestImmediateCancel_ExactlyAtPeriodEnd: canceled_at == period_end is an
// ended period too (the window is half-open). The pre-step closes it, the
// cancel lands on the new period's first instant, and that period, of which
// nothing ran, is not billed.
func TestImmediateCancel_ExactlyAtPeriodEnd(t *testing.T) {
	r := newCancelRig(t, "991 cancel exactly at period end")
	p0, p1 := r.f.cycleStart, r.f.cycleEnd

	canceled, credited, err := r.f.subSvc.Cancel(r.at(p1), r.f.tenantID, r.f.subID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if canceled.Status != domain.SubscriptionCanceled || !canceled.CanceledAt.Equal(p1) || credited != 0 {
		t.Fatalf("cancel: status=%s canceled_at=%v credited=%dc, want canceled at %v with no credit", canceled.Status, canceled.CanceledAt, credited, p1)
	}
	r.runWritesNothing(t, p1.Add(time.Hour))

	invs, lines := r.liveInvoices(t)
	if len(invs) != 1 {
		t.Fatalf("live invoices = %d, want exactly 1", len(invs))
	}
	inv := invoiceFor(t, invs, p0, p1)
	if usage, base := sumLines(lines[inv.ID]); usage != 1000 || base != cancelFixtureBaseCents || inv.SubtotalCents != 1000+cancelFixtureBaseCents {
		t.Errorf("usage=%dc base=%dc subtotal=%dc, want 1000 + %d", usage, base, inv.SubtotalCents, cancelFixtureBaseCents)
	}
	assertUsageBilledOnce(t, r.base)
}

// TestImmediateCancel_AlreadyCanceled_StillRefused: a second cancel, after
// the period end, of a subscription canceled mid-period. It answers the same
// "already terminated" error as before. The pre-step's return of an
// already-canceled row is only for a cancel that found the subscription
// active.
func TestImmediateCancel_AlreadyCanceled_StillRefused(t *testing.T) {
	r := newCancelRig(t, "991 cancel twice")
	first := r.f.cycleStart.Add(15 * 24 * time.Hour)
	if _, _, err := r.f.subSvc.Cancel(r.at(first), r.f.tenantID, r.f.subID); err != nil {
		t.Fatalf("first cancel: %v", err)
	}
	before, _ := r.liveInvoices(t)

	_, _, err := r.f.subSvc.Cancel(r.at(r.f.cycleEnd.Add(time.Hour)), r.f.tenantID, r.f.subID)
	if err == nil || !errors.Is(err, errs.ErrInvalidState) || errs.Code(err) == "unbilled_period" ||
		!strings.Contains(err.Error(), "already terminated") {
		t.Fatalf("second cancel: %v (code %q), want the existing already-terminated error", err, errs.Code(err))
	}
	if sub := r.sub(t); sub.CanceledAt == nil || !sub.CanceledAt.Equal(first) {
		t.Errorf("canceled_at = %v, want the first cancel's %v", sub.CanceledAt, first)
	}
	after, _ := r.liveInvoices(t)
	if len(before) != 1 || len(after) != 1 || after[0].ID != before[0].ID || after[0].SubtotalCents != before[0].SubtotalCents {
		t.Errorf("invoices before=%d after=%d, want the one final invoice unchanged", len(before), len(after))
	}
}

// TestImmediateCancel_TrialNeverActivated_PastPeriodEnd_StillFree: a trial
// that was never activated is cancelled free (ADR-069), however far past its
// period end the cancel lands. It pins that the pre-step leaves a trialing
// subscription alone, and that the write-off sits above the refusal.
//
// Mutation seams: drop the status == active test on the pre-step → the trial
// is activated and billed. Move the write-off below the refusal → 409.
func TestImmediateCancel_TrialNeverActivated_PastPeriodEnd_StillFree(t *testing.T) {
	r := newCancelRig(t, "991 cancel lagging trial")
	r.exec(t, `UPDATE subscriptions SET status = 'trialing', trial_start_at = $1, trial_end_at = $2, activated_at = NULL WHERE id = $3`,
		r.f.cycleStart.AddDate(0, 0, -14), r.f.cycleStart, r.f.subID)
	// A trialing subscription has no billable interval yet: activation opens
	// it. Leaving the fixture's row in place would make an activation fail,
	// and hide a pre-step that wrongly attempted one.
	r.exec(t, `DELETE FROM billing_intervals WHERE subscription_id = $1`, r.f.subID)
	cancelAt := r.f.cycleEnd.Add(time.Hour)

	canceled, credited, err := r.f.subSvc.Cancel(r.at(cancelAt), r.f.tenantID, r.f.subID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if canceled.Status != domain.SubscriptionCanceled || !canceled.CanceledAt.Equal(cancelAt) || canceled.ActivatedAt != nil || credited != 0 {
		t.Fatalf("cancel: status=%s canceled_at=%v activated_at=%v credited=%dc, want canceled at %v, never activated, no credit",
			canceled.Status, canceled.CanceledAt, canceled.ActivatedAt, credited, cancelAt)
	}
	r.runWritesNothing(t, cancelAt.Add(time.Hour))
	if invs := r.f.listInvoices(t, r.base); len(invs) != 0 {
		t.Fatalf("%d invoice(s) for a never-activated trial (first subtotal %dc), want 0", len(invs), invs[0].SubtotalCents)
	}
}

// TestImmediateCancel_AtPeriodEndFlag_AfterBoundary_ReturnsCanceledRow: the
// subscription was already scheduled to cancel at the period end, the
// boundary has passed, and the operator cancels immediately. The pre-step's
// close fires the scheduled cancel at the boundary. The immediate cancel then
// finds the subscription canceled; it was active when the call started, so
// the call answers that row, not "already terminated".
//
// Mutation seam: remove the canceled-row return after the pre-step → this
// answers "already terminated".
func TestImmediateCancel_AtPeriodEndFlag_AfterBoundary_ReturnsCanceledRow(t *testing.T) {
	r := newCancelRig(t, "991 cancel after a scheduled boundary")
	p0, p1 := r.f.cycleStart, r.f.cycleEnd
	if _, err := r.f.subSvc.ScheduleCancel(r.at(p0.Add(24*time.Hour)), r.f.tenantID, r.f.subID, subscription.ScheduleCancelInput{AtPeriodEnd: true}); err != nil {
		t.Fatalf("schedule cancel at period end: %v", err)
	}

	canceled, credited, err := r.f.subSvc.Cancel(r.at(p1.Add(time.Hour)), r.f.tenantID, r.f.subID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if canceled.Status != domain.SubscriptionCanceled || canceled.CanceledAt == nil || !canceled.CanceledAt.Equal(p1) || credited != 0 {
		t.Fatalf("cancel: status=%s canceled_at=%v credited=%dc, want canceled at the scheduled boundary %v", canceled.Status, canceled.CanceledAt, credited, p1)
	}
	r.runWritesNothing(t, p1.Add(2*time.Hour))

	invs, lines := r.liveInvoices(t)
	if len(invs) != 1 {
		t.Fatalf("live invoices = %d, want exactly 1", len(invs))
	}
	inv := invoiceFor(t, invs, p0, p1)
	if usage, base := sumLines(lines[inv.ID]); usage != 1000 || base != cancelFixtureBaseCents || inv.SubtotalCents != 1000+cancelFixtureBaseCents {
		t.Errorf("usage=%dc base=%dc subtotal=%dc, want 1000 + %d", usage, base, inv.SubtotalCents, cancelFixtureBaseCents)
	}
	assertUsageBilledOnce(t, r.base)
}

// TestImmediateCancel_TestClock_AfterPeriodEnd: the same two parts on a
// subscription pinned to a test clock whose frozen time is an hour past the
// period end (an advance that has not closed the period). No time is bound by
// the caller: the cancel reads the clock. A failing bill is refused with the
// clock's next step, not "retry billing" (retry-billing refuses a clock
// subscription), and saves no billing failure. Once it can bill, the cancel
// writes the ended period's invoice at the boundary and cancels at the
// frozen time.
func TestImmediateCancel_TestClock_AfterPeriodEnd(t *testing.T) {
	r := newCancelRig(t, "991 cancel on a test clock")
	p0, p1 := r.f.cycleStart, r.f.cycleEnd
	frozen := p1.Add(time.Hour)
	clockID := postgres.NewID("vlx_tclk")
	execTx(t, r.f.db, r.base, r.f.tenantID,
		`INSERT INTO test_clocks (id, tenant_id, livemode, name, frozen_time, status) VALUES ($1, $2, false, '991 clock', $3, 'ready')`,
		clockID, r.f.tenantID, frozen)
	r.exec(t, `UPDATE customers SET test_clock_id = $1 WHERE id = $2`, clockID, r.f.customerID)
	r.exec(t, `UPDATE subscriptions SET test_clock_id = $1 WHERE id = $2`, clockID, r.f.subID)
	r.f.engine.SetTestClockReader(testclock.NewPostgresStore(r.f.db))
	r.f.subSvc.SetResolver(r.f.engine)

	r.f.invAdapter.createTxErr = errors.New("invoice insert refused (test)")
	_, _, err := r.f.subSvc.Cancel(r.base, r.f.tenantID, r.f.subID)
	wantUnbilledPeriod(t, err)
	if !strings.Contains(err.Error(), "finish or retry the clock advance, then cancel again") {
		t.Fatalf("refusal on a clock subscription = %v, want the clock's next step", err)
	}
	if sub := r.sub(t); sub.Status != domain.SubscriptionActive || sub.BillingFailure != nil {
		t.Fatalf("after the refusal: status=%s billing_failure=%+v, want active and none (a clock subscription saves no failure)", sub.Status, sub.BillingFailure)
	}
	if n := len(r.f.listInvoices(t, r.base)); n != 0 {
		t.Fatalf("%d invoices after a refused cancel, want 0", n)
	}

	r.f.invAdapter.createTxErr = nil
	canceled, _, err := r.f.subSvc.Cancel(r.base, r.f.tenantID, r.f.subID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if canceled.Status != domain.SubscriptionCanceled || canceled.CanceledAt == nil || !canceled.CanceledAt.Equal(frozen) {
		t.Fatalf("cancel: status=%s canceled_at=%v, want canceled at the clock's frozen time %v", canceled.Status, canceled.CanceledAt, frozen)
	}
	invs, lines := r.liveInvoices(t)
	if len(invs) != 1 {
		t.Fatalf("live invoices = %d, want exactly 1", len(invs))
	}
	inv := invoiceFor(t, invs, p0, p1)
	usage, base := sumLines(lines[inv.ID])
	if usage != 1000 || base != cancelFixtureBaseCents || inv.SubtotalCents != 1000+cancelFixtureBaseCents {
		t.Errorf("usage=%dc base=%dc subtotal=%dc, want 1000 + %d", usage, base, inv.SubtotalCents, cancelFixtureBaseCents)
	}
	if !inv.IsSimulated || !inv.IssuedAtOrZero().Equal(p1) {
		t.Errorf("is_simulated=%v issued_at=%v, want a simulated invoice issued at the boundary %v", inv.IsSimulated, inv.IssuedAtOrZero(), p1)
	}
	assertUsageBilledOnce(t, r.base)
}
