package invoice

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// Fakes keyed by id, each counting its reads, so the test can pin both what a
// list page decides per row and how many reads the page costs.

type countingPMReader struct {
	ready map[string]bool
	calls int
}

func (r *countingPMReader) PaymentReadyCustomers(_ context.Context, _ string, ids []string) (map[string]bool, error) {
	r.calls++
	out := map[string]bool{}
	for _, id := range ids {
		if r.ready[id] {
			out[id] = true
		}
	}
	return out, nil
}

type countingCustomers struct {
	email map[string]string
	calls int
}

func (r *countingCustomers) Get(_ context.Context, _, id string) (domain.Customer, error) {
	return domain.Customer{ID: id, Email: r.email[id]}, nil
}

func (r *countingCustomers) GetMany(_ context.Context, _ string, ids []string) ([]domain.Customer, error) {
	r.calls++
	out := make([]domain.Customer, 0, len(ids))
	for _, id := range ids {
		out = append(out, domain.Customer{ID: id, Email: r.email[id]})
	}
	return out, nil
}

type countingDunning struct {
	escalated map[string]bool
	calls     int
}

func (r *countingDunning) ListRunsByInvoices(_ context.Context, _ string, ids []string) ([]domain.InvoiceDunningRun, error) {
	r.calls++
	var out []domain.InvoiceDunningRun
	for _, id := range ids {
		if r.escalated[id] {
			out = append(out, domain.InvoiceDunningRun{InvoiceID: id, State: domain.DunningEscalated})
		}
	}
	return out, nil
}

type countingPauses struct {
	paused map[string]bool
	calls  int
}

func (r *countingPauses) Get(_ context.Context, _, id string) (domain.Subscription, error) {
	return domain.Subscription{ID: id}, nil
}

func (r *countingPauses) PauseCollections(_ context.Context, _ string, ids []string) (map[string]*domain.PauseCollection, error) {
	r.calls++
	out := map[string]*domain.PauseCollection{}
	for _, id := range ids {
		if r.paused[id] {
			out[id] = &domain.PauseCollection{Behavior: domain.PauseCollectionKeepAsDraft}
		}
	}
	return out, nil
}

type countingPages struct {
	live  map[string]bool
	calls int
}

func (r *countingPages) PaymentPagesLive(_ context.Context, _ string, ids []string) (map[string]bool, error) {
	r.calls++
	out := map[string]bool{}
	for _, id := range ids {
		if r.live[id] {
			out[id] = true
		}
	}
	return out, nil
}

type countingStripe struct{ calls int }

func (r *countingStripe) HasFor(context.Context, string, bool) bool {
	r.calls++
	return true
}

// TestList_AttentionReadsOncePerPage pins issue #1142: a list page reads each
// attention fact once for the whole page, not once per row. Before the fix a
// 25-row page made ~3 reads per row (payment setup = customer + payment
// methods, Stripe credentials) plus up to 4 more per unpaid row, ~110 ms.
//
// And it pins that batching changed no row's banner: each row's Attention
// equals the classifier fed the facts the per-row reads used to fetch, under
// the per-row guards they used (an independent restatement, not the code
// under test).
func TestList_AttentionReadsOncePerPage(t *testing.T) {
	ctx := postgres.WithLivemode(context.Background(), false)
	store := newMemStore()

	pms := &countingPMReader{ready: map[string]bool{}}
	custs := &countingCustomers{email: map[string]string{}}
	runs := &countingDunning{escalated: map[string]bool{}}
	pauses := &countingPauses{paused: map[string]bool{}}
	pages := &countingPages{live: map[string]bool{}}
	stripe := &countingStripe{}

	// 25 rows cycling through every state a batched fact decides.
	statesFor := func(i int) (domain.InvoiceStatus, domain.InvoicePaymentStatus) {
		switch i % 5 {
		case 0:
			return domain.InvoicePaid, domain.PaymentSucceeded
		case 1:
			return domain.InvoiceDraft, domain.PaymentPending
		case 2:
			return domain.InvoiceFinalized, domain.PaymentFailed
		default:
			return domain.InvoiceFinalized, domain.PaymentPending
		}
	}
	for i := 0; i < 25; i++ {
		status, pay := statesFor(i)
		custID := fmt.Sprintf("cus_%d", i)
		subID := fmt.Sprintf("sub_%d", i)
		inv := domain.Invoice{
			ID: fmt.Sprintf("inv_%02d", i), TenantID: "t1", CustomerID: custID, SubscriptionID: subID,
			InvoiceNumber: fmt.Sprintf("INV-%02d", i), Status: status, PaymentStatus: pay,
			Currency: "USD", AmountDueCents: 1000, AutoChargePending: i%7 == 0,
		}
		store.invoices[inv.ID] = inv
		if i%3 == 0 || i%7 == 0 { // i=14: card on file AND queued
			pms.ready[custID] = true
		}
		if i%2 == 0 {
			custs.email[custID] = custID + "@acme.test"
		}
		if i%4 == 0 {
			runs.escalated[inv.ID] = true
		}
		if i%2 == 0 {
			pauses.paused[subID] = true
		}
		if i%8 == 3 {
			pages.live[inv.ID] = true
		}
	}

	svc := NewService(store, nil, nil)
	svc.SetPaymentMethodReader(pms)
	svc.SetCustomerReader(custs)
	svc.SetDunningRunReader(runs)
	svc.SetSubscriptionPauseReader(pauses)
	svc.SetPaymentPageReader(pages)
	svc.SetStripeChecker(stripe)

	got, _, err := svc.List(ctx, ListFilter{TenantID: "t1"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 25 {
		t.Fatalf("rows = %d, want 25", len(got))
	}

	for name, calls := range map[string]int{
		"payment readiness": pms.calls, "customer email": custs.calls, "dunning runs": runs.calls,
		"pause state": pauses.calls, "payment pages": pages.calls, "stripe credentials": stripe.calls,
	} {
		if calls != 1 {
			t.Errorf("%s: %d reads for a 25-row page, want 1", name, calls)
		}
	}

	for _, inv := range got {
		raw := store.invoices[inv.ID]
		// The pre-#1142 per-row rules, restated.
		atc := domain.AttentionContext{StripeConnected: true}
		atc.HasPaymentMethod = pms.ready[raw.CustomerID]
		awaitsNoCard := !atc.HasPaymentMethod && raw.Status == domain.InvoiceFinalized && raw.PaymentStatus == domain.PaymentPending
		if awaitsNoCard {
			atc.CustomerHasEmail = custs.email[raw.CustomerID] != ""
			atc.DunningEscalated = runs.escalated[raw.ID]
		}
		if (raw.AutoChargePending || !atc.HasPaymentMethod) && raw.Status == domain.InvoiceFinalized && raw.PaymentStatus == domain.PaymentPending {
			atc.CollectionPaused = pauses.paused[raw.SubscriptionID]
		}
		if raw.Status == domain.InvoiceFinalized && (raw.PaymentStatus == domain.PaymentPending || raw.PaymentStatus == domain.PaymentFailed) {
			atc.PaymentPageOpen = pages.live[raw.ID]
		}
		want := domain.ClassifyInvoiceAttention(raw, atc)
		if !reflect.DeepEqual(inv.Attention, want) {
			t.Errorf("%s (%s/%s): attention = %+v, want %+v", inv.ID, raw.Status, raw.PaymentStatus, inv.Attention, want)
		}
	}
}

// A single read is a page of one: Get shows the same banner the list row does.
func TestGet_AttentionMatchesListRow(t *testing.T) {
	ctx := postgres.WithLivemode(context.Background(), false)
	store := newMemStore()
	store.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", CustomerID: "cus_1", SubscriptionID: "sub_1", InvoiceNumber: "INV-1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending, Currency: "USD", AmountDueCents: 1000,
	}
	svc := NewService(store, nil, nil)
	svc.SetCustomerReader(&countingCustomers{email: map[string]string{"cus_1": "a@acme.test"}})
	svc.SetDunningRunReader(&countingDunning{escalated: map[string]bool{"inv_1": true}})
	svc.SetSubscriptionPauseReader(&countingPauses{paused: map[string]bool{"sub_1": true}})

	one, err := svc.Get(ctx, "t1", "inv_1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	list, _, err := svc.List(ctx, ListFilter{TenantID: "t1"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// Escalated run + paused subscription + email on file: every fact the
	// page-of-one read reached the banner.
	if one.Attention == nil || one.Attention.Code != "payment.dunning_exhausted" ||
		!strings.Contains(one.Attention.Message, "collection is paused") ||
		len(one.Attention.Actions) == 0 || one.Attention.Actions[0].Code != domain.AttentionActionSendReminder {
		t.Fatalf("get attention = %+v, want dunning_exhausted, paused copy, Resend first", one.Attention)
	}
	if !reflect.DeepEqual(one.Attention, list[0].Attention) {
		t.Fatalf("get attention %+v != list row attention %+v", one.Attention, list[0].Attention)
	}
}
