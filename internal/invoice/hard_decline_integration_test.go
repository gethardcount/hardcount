package invoice_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/paymentmethods"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// #966 on the real store: a failure report's decline code and card land on
// the attempt row in the report's transaction, a source that does not know
// them never erases them, and the two reads — "was this card refused for good
// on this invoice?" for the dunning retry, "is the customer's default card
// one of those?" for the attention notice — answer from the attempt rows,
// with Postgres row-level security keeping each tenant to its own.

type hardDeclineStoreRig struct {
	t        *testing.T
	ctx      context.Context
	tenantID string
	store    *invoice.PostgresStore
	custs    *customer.PostgresStore
	pms      *paymentmethods.PostgresStore
}

func (r *hardDeclineStoreRig) customer(key string) string {
	r.t.Helper()
	c, err := r.custs.Create(r.ctx, r.tenantID, domain.Customer{ExternalID: "ext_" + key, DisplayName: key})
	if err != nil {
		r.t.Fatalf("create customer: %v", err)
	}
	return c.ID
}

func (r *hardDeclineStoreRig) card(custID, pm string) paymentmethods.PaymentMethod {
	r.t.Helper()
	got, err := r.pms.Upsert(r.ctx, r.tenantID, paymentmethods.PaymentMethod{
		CustomerID: custID, StripePaymentMethodID: pm, Type: "card", CardFingerprint: "fp_" + pm,
	})
	if err != nil {
		r.t.Fatalf("add card %s: %v", pm, err)
	}
	return got
}

func (r *hardDeclineStoreRig) invoice(custID, num string) string {
	r.t.Helper()
	inv, err := r.store.Create(r.ctx, r.tenantID, domain.Invoice{
		CustomerID: custID, InvoiceNumber: num, Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		Currency: "USD", SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
		BillingPeriodStart: time.Now().UTC().Add(-30 * 24 * time.Hour), BillingPeriodEnd: time.Now().UTC(),
	})
	if err != nil {
		r.t.Fatalf("create invoice: %v", err)
	}
	return inv.ID
}

// decline reports a failure the way the charge call does (its own seq).
func (r *hardDeclineStoreRig) decline(invID, pi, code, pm string) {
	r.t.Helper()
	cur, err := r.store.Get(r.ctx, r.tenantID, invID)
	if err != nil {
		r.t.Fatalf("get: %v", err)
	}
	seq := cur.ChargeAttemptSeq
	if _, res, err := r.store.MarkPaymentFailedReportingTransition(r.ctx, r.tenantID, invID, domain.PaymentFailureReport{
		PaymentIntentID: pi, Message: "Your card was declined.", DeclineCode: code, PaymentMethodID: pm, ChargedAtSeq: &seq,
	}, nil); err != nil || !res.Recorded {
		r.t.Fatalf("report %s: recorded=%v err=%v", pi, res.Recorded, err)
	}
}

func (r *hardDeclineStoreRig) attempt(invID, pi string) domain.InvoiceChargeAttempt {
	r.t.Helper()
	rows, err := r.store.ListChargeAttemptsByInvoice(r.ctx, r.tenantID, invID)
	if err != nil {
		r.t.Fatalf("list attempts: %v", err)
	}
	for _, a := range rows {
		if a.StripePaymentIntentID == pi {
			return a
		}
	}
	r.t.Fatalf("no attempt row for %s", pi)
	return domain.InvoiceChargeAttempt{}
}

func (r *hardDeclineStoreRig) refused(invID, pm string) string {
	r.t.Helper()
	code, err := r.store.HardDeclineOnCard(r.ctx, r.tenantID, invID, pm)
	if err != nil {
		r.t.Fatalf("HardDeclineOnCard: %v", err)
	}
	return code
}

func TestHardDecline_AttemptRowAndReads_RealStore(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	r := &hardDeclineStoreRig{
		t: t, ctx: ctx, tenantID: testutil.CreateTestTenant(t, db, "Hard Decline Store"),
		store: invoice.NewPostgresStore(db), custs: customer.NewPostgresStore(db), pms: paymentmethods.NewPostgresStore(db),
	}

	cust := r.customer("a")
	r.card(cust, "pm_a") // first card: the default
	inv := r.invoice(cust, "INV-HDS-1")

	// The charge call records its attempt (the card, no code yet) and then
	// reports the decline with both.
	if err := r.store.RecordChargeAttempt(ctx, r.tenantID, domain.InvoiceChargeAttempt{
		InvoiceID: inv, StripePaymentIntentID: "pi_a1", Trigger: domain.ChargeTriggerAutoCharge,
		Outcome: domain.ChargeAttemptFailed, ProviderReason: "Your card was declined.",
		StripePaymentMethodID: "pm_a", AmountCents: 5000, OccurredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("record attempt: %v", err)
	}
	r.decline(inv, "pi_a1", "lost_card", "pm_a")
	if a := r.attempt(inv, "pi_a1"); a.DeclineCode != "lost_card" || a.StripePaymentMethodID != "pm_a" || a.Trigger != domain.ChargeTriggerAutoCharge {
		t.Fatalf("attempt row %+v, want lost_card on pm_a, trigger kept", a)
	}

	// A later source that does not know them — the reconciler, a redelivered
	// webhook without last_payment_error, the chokepoint's own upsert —
	// leaves them as they are.
	if _, _, err := r.store.MarkPaymentFailedReportingTransition(ctx, r.tenantID, inv,
		domain.PaymentFailureReport{PaymentIntentID: "pi_a1", Message: "reconciled: requires_payment_method"}, nil); err != nil {
		t.Fatalf("re-report: %v", err)
	}
	if err := r.store.RecordChargeAttempt(ctx, r.tenantID, domain.InvoiceChargeAttempt{
		InvoiceID: inv, StripePaymentIntentID: "pi_a1", Trigger: domain.ChargeTriggerAutoCharge,
		Outcome: domain.ChargeAttemptFailed, AmountCents: 5000, OccurredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("record attempt again: %v", err)
	}
	if a := r.attempt(inv, "pi_a1"); a.DeclineCode != "lost_card" || a.StripePaymentMethodID != "pm_a" {
		t.Fatalf("after sources that did not know the decline: %+v, want lost_card on pm_a kept", a)
	}

	// The retry's read: per invoice and per card.
	if got := r.refused(inv, "pm_a"); got != "lost_card" {
		t.Errorf("refused(inv, pm_a) = %q, want lost_card", got)
	}
	if got := r.refused(inv, "pm_other"); got != "" {
		t.Errorf("refused(inv, pm_other) = %q, want empty — another card is charged", got)
	}
	if got := r.refused(inv, ""); got != "" {
		t.Errorf("refused(inv, \"\") = %q, want empty", got)
	}
	otherInv := r.invoice(cust, "INV-HDS-2")
	if got := r.refused(otherInv, "pm_a"); got != "" {
		t.Errorf("refused(another invoice, pm_a) = %q, want empty — the rule is per invoice, as Stripe's", got)
	}

	// An ordinary decline on that other invoice is no refusal.
	r.decline(otherInv, "pi_b1", "insufficient_funds", "pm_a")
	if got := r.refused(otherInv, "pm_a"); got != "" {
		t.Errorf("refused after insufficient_funds = %q, want empty", got)
	}

	// The attention read, over a page: inv (refused on its default card),
	// otherInv (ordinary decline), and a third customer's invoice refused on
	// a card that is NOT their default.
	cust3 := r.customer("c")
	r.card(cust3, "pm_c_default")
	r.card(cust3, "pm_c_old") // added beside the default (ADR-021)
	inv3 := r.invoice(cust3, "INV-HDS-3")
	r.decline(inv3, "pi_c1", "stolen_card", "pm_c_old")
	page := []string{inv, otherInv, inv3}
	got, err := r.store.HardDeclinesOnDefaultCards(ctx, r.tenantID, page)
	if err != nil {
		t.Fatalf("HardDeclinesOnDefaultCards: %v", err)
	}
	if len(got) != 1 || got[inv] != "lost_card" {
		t.Errorf("HardDeclinesOnDefaultCards = %v, want only %s → lost_card", got, inv)
	}

	// The card is replaced: a new card becomes the default, and the refused
	// card is no longer what the next retry would charge.
	fresh := r.card(cust, "pm_a_new")
	if _, err := r.pms.SetDefault(ctx, r.tenantID, cust, fresh.ID); err != nil {
		t.Fatalf("set default: %v", err)
	}
	if got, err := r.store.HardDeclinesOnDefaultCards(ctx, r.tenantID, page); err != nil || len(got) != 0 {
		t.Errorf("after the card was replaced: %v (err %v), want none", got, err)
	}
	if got := r.refused(inv, "pm_a"); got != "lost_card" {
		t.Errorf("refused(inv, pm_a) after replacement = %q, want lost_card still — making it the default again must not retry it", got)
	}

	// Row-level security: another tenant sees none of it, and the queries
	// carry no tenant filter of their own.
	otherTenant := testutil.CreateTestTenant(t, db, "Hard Decline Other Tenant")
	if code, err := r.store.HardDeclineOnCard(ctx, otherTenant, inv, "pm_a"); err != nil || code != "" {
		t.Errorf("other tenant HardDeclineOnCard = %q, %v, want empty", code, err)
	}
	// Put the refused card back as the default so the own-tenant read has a
	// row to find; the other tenant must still see nothing.
	if _, err := r.pms.SetDefault(ctx, r.tenantID, cust, r.findPM(cust, "pm_a").ID); err != nil {
		t.Fatalf("restore default: %v", err)
	}
	if got, err := r.store.HardDeclinesOnDefaultCards(ctx, otherTenant, page); err != nil || len(got) != 0 {
		t.Errorf("other tenant HardDeclinesOnDefaultCards = %v, %v, want none", got, err)
	}
	if got, err := r.store.HardDeclinesOnDefaultCards(ctx, r.tenantID, page); err != nil || got[inv] != "lost_card" {
		t.Errorf("own tenant after restoring the refused default = %v, %v, want %s → lost_card", got, err, inv)
	}
}

func (r *hardDeclineStoreRig) findPM(custID, stripePM string) paymentmethods.PaymentMethod {
	r.t.Helper()
	list, err := r.pms.List(r.ctx, r.tenantID, custID)
	if err != nil {
		r.t.Fatalf("list cards: %v", err)
	}
	for _, pm := range list {
		if pm.StripePaymentMethodID == stripePM {
			return pm
		}
	}
	r.t.Fatalf("no card %s", stripePM)
	return paymentmethods.PaymentMethod{}
}
