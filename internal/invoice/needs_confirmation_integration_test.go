package invoice_test

import (
	"context"
	"testing"

	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/paymentmethods"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// The two reads that stop a dunning retry from charging a card, on the real
// store: authentication_required (the bank wants the customer to confirm the
// payment) on the card's newest decline stops it, and a later ordinary
// decline is the newer answer and ends it; a hard decline on the card stops
// it whatever came after. The per-invoice read (the retry) and the page read
// (the notice, for the default card) agree.
func TestRetryBlockingDecline_ConfirmIsTheNewestAnswerAHardDeclineIsFinal_RealStore(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	r := &hardDeclineStoreRig{
		t: t, ctx: ctx, tenantID: testutil.CreateTestTenant(t, db, "Retry Block Store"),
		store: invoice.NewPostgresStore(db), custs: customer.NewPostgresStore(db), pms: paymentmethods.NewPostgresStore(db),
	}
	pageRead := func(ids ...string) map[string]string {
		t.Helper()
		got, err := r.store.RetryBlockingDeclinesOnDefaultCards(ctx, r.tenantID, ids)
		if err != nil {
			t.Fatalf("RetryBlockingDeclinesOnDefaultCards: %v", err)
		}
		return got
	}

	// Request to confirm, then an ordinary decline on the same card.
	custA := r.customer("a")
	r.card(custA, "pm_a")
	invA := r.invoice(custA, "INV-RB-1")
	r.decline(invA, "pi_a1", "authentication_required", "pm_a")
	if code := r.refused(invA, "pm_a"); code != "authentication_required" {
		t.Errorf("after the request to confirm: %q, want authentication_required", code)
	}
	if got := pageRead(invA); got[invA] != "authentication_required" {
		t.Errorf("page read after the request to confirm: %v, want authentication_required", got)
	}
	r.decline(invA, "pi_a2", "insufficient_funds", "pm_a")
	if code := r.refused(invA, "pm_a"); code != "" {
		t.Errorf("after a later ordinary decline: %q, want none (the newer answer decides)", code)
	}
	if got := pageRead(invA); len(got) != 0 {
		t.Errorf("page read after a later ordinary decline: %v, want none", got)
	}

	// A hard decline, then a request to confirm: the refusal stays final.
	custB := r.customer("b")
	r.card(custB, "pm_b")
	invB := r.invoice(custB, "INV-RB-2")
	r.decline(invB, "pi_b1", "stolen_card", "pm_b")
	r.decline(invB, "pi_b2", "authentication_required", "pm_b")
	if code := r.refused(invB, "pm_b"); code != "stolen_card" {
		t.Errorf("hard decline then confirm: %q, want stolen_card", code)
	}
	if got := pageRead(invB); got[invB] != "stolen_card" {
		t.Errorf("page read, hard decline then confirm: %v, want stolen_card", got)
	}

	// A request to confirm, then a hard decline: the refusal decides.
	custC := r.customer("c")
	r.card(custC, "pm_c")
	invC := r.invoice(custC, "INV-RB-3")
	r.decline(invC, "pi_c1", "authentication_required", "pm_c")
	r.decline(invC, "pi_c2", "lost_card", "pm_c")
	if code := r.refused(invC, "pm_c"); code != "lost_card" {
		t.Errorf("confirm then hard decline: %q, want lost_card", code)
	}

	// The request is about that card: another card is charged as usual, and
	// once another card is the default the notice no longer reads it.
	custD := r.customer("d")
	r.card(custD, "pm_d")
	invD := r.invoice(custD, "INV-RB-4")
	r.decline(invD, "pi_d1", "authentication_required", "pm_d")
	if code := r.refused(invD, "pm_other"); code != "" {
		t.Errorf("another card: %q, want none", code)
	}
	if _, _, err := r.pms.Upsert(ctx, r.tenantID, paymentmethods.PaymentMethod{
		CustomerID: custD, StripePaymentMethodID: "pm_d2", Type: "card", CardFingerprint: "fp_pm_d2",
	}, true); err != nil {
		t.Fatalf("add default card: %v", err)
	}
	if got := pageRead(invD); len(got) != 0 {
		t.Errorf("page read once another card is the default: %v, want none", got)
	}
}
