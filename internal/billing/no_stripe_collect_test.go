package billing

import (
	"context"
	"errors"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// fakeStripeReadiness answers HasFor with connected, or with err.
type fakeStripeReadiness struct {
	connected bool
	err       error
}

func (f fakeStripeReadiness) HasFor(context.Context, string, bool) (bool, error) {
	return f.connected, f.err
}

// switchableStripeReadiness lets a test connect Stripe between sweeps.
type switchableStripeReadiness struct {
	connected bool
	modes     []bool
}

func (f *switchableStripeReadiness) HasFor(_ context.Context, _ string, livemode bool) (bool, error) {
	f.modes = append(f.modes, livemode)
	return f.connected, nil
}

// fakeInvoiceEmailer records each invoice email the collector sends and
// answers with outcome (NotifySent when empty) or err.
type fakeInvoiceEmailer struct {
	sent    []string
	outcome domain.NotifyOutcome
	err     error
}

func (f *fakeInvoiceEmailer) EmailInvoice(_ context.Context, _, invoiceID string) (domain.NotifyOutcome, error) {
	if f.err != nil {
		return "", f.err
	}
	f.sent = append(f.sent, invoiceID)
	if f.outcome == "" {
		return domain.NotifySent, nil
	}
	return f.outcome, nil
}

type noStripeHarness struct {
	engine   *Engine
	invoices *mockInvoices
	ready    *switchableStripeReadiness
	emailer  *fakeInvoiceEmailer
	links    *triggerNoPMNotifier
	starter  *recordingDunningStarter
}

func newNoStripeHarness(t *testing.T) noStripeHarness {
	t.Helper()
	h := noStripeHarness{
		invoices: &mockInvoices{invoices: []domain.Invoice{pendingInvoice()}},
		ready:    &switchableStripeReadiness{},
		emailer:  &fakeInvoiceEmailer{},
		links:    &triggerNoPMNotifier{},
		starter:  &recordingDunningStarter{},
	}
	h.engine = noPMEngine(t, h.invoices)
	h.engine.SetStripeReadiness(h.ready)
	h.engine.SetInvoiceEmailer(h.emailer)
	h.engine.SetNoPaymentMethodNotifier(h.links)
	h.engine.SetDunningStarter(h.starter)
	return h
}

func (h noStripeHarness) sweep(t *testing.T) []error {
	t.Helper()
	_, errs := h.engine.RetryPendingCharges(postgres.WithLivemode(context.Background(), true), 10, nil)
	return errs
}

// Without Stripe for the invoice's mode, an owed invoice with no card is
// collected like a send-invoice tenant's (ADR-060 amendment 2026-10-30): the
// customer is emailed the invoice once, never a setup link, and the
// no-payment dunning run starts so the invoice still reaches the policy's
// final action. The second sweep sends nothing more. Mutations: drop the
// !connected guard (the setup link goes out), drop the invoice_emailed_at
// guard (two invoice emails), skip the stamp (two invoice emails).
func TestCollector_NoStripe_EmailsInvoiceOnceAndStartsDunning(t *testing.T) {
	h := newNoStripeHarness(t)

	for i := 0; i < 2; i++ {
		if errs := h.sweep(t); len(errs) != 0 {
			t.Fatalf("sweep %d: errors %v", i+1, errs)
		}
	}
	if len(h.emailer.sent) != 1 || h.emailer.sent[0] != "inv_1" {
		t.Fatalf("invoice emails %v, want one for inv_1 across two sweeps", h.emailer.sent)
	}
	if h.invoices.invoices[0].InvoiceEmailedAt == nil {
		t.Fatal("invoice_emailed_at not stamped after the invoice email was sent")
	}
	if len(h.links.triggers) != 0 || h.invoices.invoices[0].NoPMNotifiedAt != nil {
		t.Fatalf("setup links %v (no_pm_notified_at %v), want none without Stripe", h.links.triggers, h.invoices.invoices[0].NoPMNotifiedAt)
	}
	if len(h.starter.started) == 0 || h.starter.started[0] != "inv_1" || h.starter.causes[0] != domain.DunningCauseNoPaymentMethod {
		t.Fatalf("dunning starts %v causes %v, want inv_1 with cause no_payment_method", h.starter.started, h.starter.causes)
	}
	if !h.invoices.invoices[0].AutoChargePending || h.invoices.holdSet("inv_1") {
		t.Fatalf("queued %v, hold set %v; want still queued with the hold released",
			h.invoices.invoices[0].AutoChargePending, h.invoices.holdSet("inv_1"))
	}
	for _, live := range h.ready.modes {
		if !live {
			t.Fatalf("readiness asked for modes %v, want the sweep's livemode (true)", h.ready.modes)
		}
	}
}

// Connecting Stripe later puts the invoice back on today's path: the next
// visit sends the setup link once, and the invoice email is not sent again.
func TestCollector_NoStripe_ConnectLaterSendsSetupLinkOnce(t *testing.T) {
	h := newNoStripeHarness(t)
	if errs := h.sweep(t); len(errs) != 0 {
		t.Fatalf("sweep without Stripe: %v", errs)
	}
	h.ready.connected = true
	for i := 0; i < 2; i++ {
		if errs := h.sweep(t); len(errs) != 0 {
			t.Fatalf("sweep %d with Stripe: %v", i+1, errs)
		}
	}
	if len(h.links.triggers) != 1 {
		t.Fatalf("setup links %v, want one once Stripe is connected", h.links.triggers)
	}
	if len(h.emailer.sent) != 1 {
		t.Fatalf("invoice emails %v, want only the one sent before Stripe was connected", h.emailer.sent)
	}
}

// A failed read of the Stripe connection is not "not connected" (no silent
// fallbacks): the sweep counts an error wrapping the cause, nothing is sent,
// no dunning starts, the hold is released and the invoice stays queued.
// A ctx with no mode is the same: an error, never a guess. Mutation: treat
// the read error as not connected (the invoice email goes out and dunning
// starts).
func TestCollector_StripeReadError_SendsAndStartsNothing(t *testing.T) {
	blip := errors.New("credentials read failed")
	cases := []struct {
		name string
		ctx  context.Context
		err  error
	}{
		{"read error", postgres.WithLivemode(context.Background(), true), blip},
		{"no mode on ctx", context.Background(), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newNoStripeHarness(t)
			h.engine.SetStripeReadiness(fakeStripeReadiness{err: tc.err})
			_, errs := h.engine.RetryPendingCharges(tc.ctx, 10, nil)
			if tc.err != nil && (len(errs) != 1 || !errors.Is(errs[0], blip)) {
				t.Fatalf("errors %v, want one wrapping the read failure", errs)
			}
			if tc.err == nil && len(errs) == 0 {
				t.Fatal("no error for a ctx with no mode, want one")
			}
			if len(h.emailer.sent) != 0 || len(h.links.triggers) != 0 || len(h.starter.causes) != 0 {
				t.Fatalf("invoice emails %v, setup links %v, dunning starts %v; want none",
					h.emailer.sent, h.links.triggers, h.starter.causes)
			}
			if tc.err != nil && (h.invoices.holdSet("inv_1") || !h.invoices.invoices[0].AutoChargePending) {
				t.Fatal("want the hold released and the invoice still queued")
			}
		})
	}
}

// An invoice email that is refused (no SMTP on this server) or skipped (no
// customer email) is left unstamped, so the next visit tries again; the
// dunning run starts either way. Mutation: stamp on any outcome (the second
// sweep sends nothing).
func TestCollector_NoStripe_UnsentInvoiceEmailIsRetried(t *testing.T) {
	cases := []struct {
		name    string
		emailer *fakeInvoiceEmailer
	}{
		{"refused", &fakeInvoiceEmailer{err: errors.New("email isn't set up on this server")}},
		{"no customer email", &fakeInvoiceEmailer{outcome: domain.NotifySkippedNoEmail}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newNoStripeHarness(t)
			h.engine.SetInvoiceEmailer(tc.emailer)
			for i := 0; i < 2; i++ {
				if errs := h.sweep(t); len(errs) != 0 {
					t.Fatalf("sweep %d: errors %v", i+1, errs)
				}
			}
			if h.invoices.invoices[0].InvoiceEmailedAt != nil {
				t.Fatal("invoice_emailed_at stamped although the email was not sent")
			}
			if tc.emailer.err == nil && len(tc.emailer.sent) != 2 {
				t.Fatalf("invoice email attempts %v, want one per sweep", tc.emailer.sent)
			}
			if len(h.starter.started) == 0 || len(h.links.triggers) != 0 {
				t.Fatalf("dunning starts %v, setup links %v; want the run started and no link", h.starter.started, h.links.triggers)
			}
		})
	}
}

// With Stripe connected the path is as before: the setup link once, and no
// invoice email from the collector.
func TestCollector_StripeConnected_SetupLinkNoInvoiceEmail(t *testing.T) {
	h := newNoStripeHarness(t)
	h.ready.connected = true
	for i := 0; i < 2; i++ {
		if errs := h.sweep(t); len(errs) != 0 {
			t.Fatalf("sweep %d: %v", i+1, errs)
		}
	}
	if len(h.links.triggers) != 1 || len(h.emailer.sent) != 0 {
		t.Fatalf("setup links %v, invoice emails %v; want one link and no invoice email", h.links.triggers, h.emailer.sent)
	}
	if len(h.starter.started) == 0 {
		t.Fatal("no dunning start, want the no-payment run")
	}
}

// A card saved before Stripe was disconnected cannot be charged: the charge
// would come back "Stripe not configured", a skip the sweep repeated on every
// visit with no email and no dunning run. Without Stripe the collector takes
// the no-Stripe path whatever is on file: no charge, the invoice email once,
// and the dunning run. Mutation: check readiness only for the no-card case
// (the card is charged and nothing else happens).
func TestCollector_NoStripe_CardOnFileIsNotCharged(t *testing.T) {
	h := newNoStripeHarness(t)
	charger := &recordingCharger{}
	h.engine.charger = charger
	h.engine.paymentSetups = &fakePaymentSetups{ready: true, stripeCustomerID: "cus_stripe"}

	for i := 0; i < 2; i++ {
		if errs := h.sweep(t); len(errs) != 0 {
			t.Fatalf("sweep %d: errors %v", i+1, errs)
		}
	}
	if len(charger.got) != 0 {
		t.Fatalf("charges %d, want none without Stripe", len(charger.got))
	}
	if len(h.emailer.sent) != 1 || len(h.links.triggers) != 0 {
		t.Fatalf("invoice emails %v, setup links %v; want one invoice email and no link", h.emailer.sent, h.links.triggers)
	}
	if len(h.starter.started) == 0 || h.starter.causes[0] != domain.DunningCauseNoPaymentMethod {
		t.Fatalf("dunning starts %v, want the no-payment run", h.starter.started)
	}
	if !h.invoices.invoices[0].AutoChargePending || h.invoices.holdSet("inv_1") {
		t.Fatal("want the invoice still queued with its hold released")
	}

	// Stripe connected again: the card is charged.
	h.ready.connected = true
	if errs := h.sweep(t); len(errs) != 0 {
		t.Fatalf("sweep with Stripe: %v", errs)
	}
	if len(charger.got) != 1 {
		t.Fatalf("charges %d once Stripe is connected, want 1", len(charger.got))
	}
}
