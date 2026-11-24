package invoice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
)

// A top-up's grant runs after its payment is taken. Its error must never read
// as ErrInvalidState, which the settle records as a payment on a voided
// invoice and acknowledges, or as ErrNotFound, which the webhook
// acknowledges: either would keep the money and drop the credit. It must
// fail the settle, so the payment is retried.
func TestTopupGrantError_NeverMatchesInvalidStateOrNotFound(t *testing.T) {
	for _, cause := range []error{
		errs.InvalidState("cannot grant"),
		errs.NotFound("customer"),
		fmt.Errorf("wrapped: %w", errs.ErrNotFound),
	} {
		err := topupGrantError("hc_inv_1", cause)
		if errors.Is(err, errs.ErrInvalidState) || errors.Is(err, errs.ErrNotFound) {
			t.Errorf("grant error %v still matches %v", err, cause)
		}
		if err == nil || err.Error() == "" {
			t.Errorf("grant error for %v lost its text", cause)
		}
	}
}

type topupGrantFake struct {
	cents   int64
	expires *time.Time
}

func (f topupGrantFake) TopupGrant(context.Context, string, string) (int64, *time.Time, bool, error) {
	return f.cents, f.expires, true, nil
}

// A paid top-up's Activity reads as one: its payment issues it ("Invoice
// issued", never "finalized") and then adds the credit it sold, with the
// grant's own amount and expiry.
func TestPaymentTimeline_PaidTopupIssuedAndCreditAdded(t *testing.T) {
	paid := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	expires := paid.AddDate(1, 0, 0)
	h, id := wireHandler(t, wireCase{inv: domain.Invoice{
		Status: domain.InvoicePaid, BillingReason: domain.BillingReasonTopup, InvoiceNumber: "HC-000001",
		TotalAmountCents: 9000, AmountPaidCents: 9000, PaidOutOfBand: true,
		CreatedAt: paid.Add(-time.Hour), IssuedAt: &paid, PaidAt: &paid,
	}})
	h.topupGrants = topupGrantFake{cents: 10000, expires: &expires}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	ctx := context.WithValue(auth.WithTenantID(req.Context(), "t1"), chi.RouteCtxKey, rctx)
	rr := httptest.NewRecorder()
	h.paymentTimeline(rr, req.WithContext(ctx))
	var resp timelineResp
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %d: %v", rr.Code, err)
	}
	var titles []string
	for _, e := range resp.Events {
		titles = append(titles, toWireRow(e).Title)
	}
	want := []string{"Top-up created", "Invoice issued", "Offline payment received", "Credit added"}
	if fmt.Sprint(titles) != fmt.Sprint(want) {
		t.Fatalf("rows %q, want %q", titles, want)
	}
	added := oneRow(t, resp.Events, "credit.added")
	if added.Amount != 10000 || added.Detail != "Expires Nov 20, 2027 10:00 UTC" {
		t.Errorf("credit added row: amount %d, detail %q; want 10000 and the grant's expiry", added.Amount, added.Detail)
	}
}
