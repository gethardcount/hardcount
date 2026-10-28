package invoice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
)

// TestPaymentTimeline_RefundRowReportsOutcomeNotIntent locks the 2026-08-03
// FLOW C2 finding: the credit-note row was labelled from RefundAmountCents
// alone — what was ALLOCATED to the card — and never consulted RefundStatus,
// whether that money actually moved.
//
// Observed on CN-000027: refund_status=failed, stripe_refund_id empty, nothing
// left the Stripe balance, and the invoice timeline said "Refund issued ·
// $10.00". That is the surface an operator opens to ask what happened to an
// invoice, and it asserted the customer had been repaid when they had not.
// This is NOT specific to the offline-payment case that surfaced it: any card
// refund that fails at Stripe (declined, expired card, network) renders the
// same way, which is an ordinary production path.
//
// Every case below is one status against otherwise IDENTICAL rows, because a
// single-status test passes just as well against the old allocation-only
// labelling.
func TestPaymentTimeline_RefundRowReportsOutcomeNotIntent(t *testing.T) {
	t.Parallel()

	issuedAt := time.Date(2026, 8, 2, 15, 24, 0, 0, time.UTC)

	seed := func(t *testing.T, cn domain.CreditNote) timelineResp {
		t.Helper()
		store := newMemStore()
		inv, err := store.Create(context.Background(), "t1", domain.Invoice{
			CustomerID: "cus_1", Status: domain.InvoicePaid,
			PaymentStatus: domain.PaymentSucceeded, AmountPaidCents: 5362,
			StripePaymentIntentID: "pi_card",
		})
		if err != nil {
			t.Fatalf("seed invoice: %v", err)
		}
		cn.InvoiceID = inv.ID
		cn.TenantID = "t1"
		cn.Status = domain.CreditNoteIssued
		cn.IssuedAt = &issuedAt
		h := &Handler{svc: NewService(store, nil, nil)}
		h.creditNotes = &cnLaneFake{cns: []domain.CreditNote{cn}}

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", inv.ID)
		ctx := auth.WithTenantID(req.Context(), "t1")
		ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
		rr := httptest.NewRecorder()
		h.paymentTimeline(rr, req.WithContext(ctx))
		if rr.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
		}
		var resp timelineResp
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp
	}

	type cnRow struct{ desc, status, problem, detail string }
	rowFor := func(t *testing.T, resp timelineResp, number string) cnRow {
		t.Helper()
		for _, e := range resp.Events {
			d, _ := e["detail"].(string)
			if strings.HasPrefix(d, number) {
				desc, _ := e["description"].(string)
				status, _ := e["status"].(string)
				problem, _ := e["error"].(string)
				return cnRow{desc, status, problem, d}
			}
		}
		t.Fatalf("no timeline row for %q; events=%v", number, resp.Events)
		return cnRow{}
	}

	// Since the activity line standard (ADR-128, decision 8) every credit
	// note has one title, "Credit note issued", with its total. The refund's
	// outcome moved out of the title into the tone (the dot) and the problem
	// line — and the card part of the meta says "refunded" only when the
	// money moved. Each status is asserted on all three channels, so none
	// can claim a refund that did not happen.
	t.Run("full refund", func(t *testing.T) {
		cases := []struct {
			status      domain.RefundStatus
			wantStatus  string
			wantProblem string
			wantCard    string
		}{
			{domain.RefundSucceeded, "succeeded", "", "$10.00 refunded to card"},
			{domain.RefundFailed, "failed", "Card refund failed.", "$10.00 to card"},
			{domain.RefundPending, "warning", "The card refund is waiting on the payment provider.", "$10.00 to card"},
			// Allocated but never executed. Must not borrow the success
			// wording merely because it is not an explicit failure.
			{domain.RefundNone, "warning", "No refund was sent to the payment provider.", "$10.00 to card"},
			// Stripe gave no clear answer (#965). "Not made" would tell the
			// operator nothing moved, which invites a second refund by hand
			// in Stripe; the customer may already have been refunded.
			{domain.RefundUnknown, "warning", "The payment provider did not say whether the card refund went through.", "$10.00 to card"},
		}
		for _, tc := range cases {
			t.Run(string(tc.status), func(t *testing.T) {
				resp := seed(t, domain.CreditNote{
					CreditNoteNumber: "CN-FULL", TotalCents: 1000, Currency: "USD",
					RefundAmountCents: 1000, RefundStatus: tc.status,
				})
				got := rowFor(t, resp, "CN-FULL")
				// The status field drives the UI's verdict dot. It was once
				// hardcoded "succeeded" for every CN row, so a failed refund
				// wore an emerald success dot beside the words "Refund
				// failed" (2026-08-04 invoice census).
				if got.status != tc.wantStatus {
					t.Errorf("refund_status=%q: timeline status %q, want %q — the verdict dot must agree with the words", tc.status, got.status, tc.wantStatus)
				}
				if got.desc != "Credit note issued" || got.problem != tc.wantProblem {
					t.Errorf("refund_status=%q rendered %q / problem %q, want %q / %q", tc.status, got.desc, got.problem, "Credit note issued", tc.wantProblem)
				}
				if got.detail != "CN-FULL · "+tc.wantCard {
					t.Errorf("refund_status=%q detail %q, want %q", tc.status, got.detail, "CN-FULL · "+tc.wantCard)
				}
			})
		}
	})

	// Partial refund: the note also carries a credit-balance part, and the
	// card part must still be honest.
	t.Run("partial refund", func(t *testing.T) {
		for _, tc := range []struct {
			status     domain.RefundStatus
			wantDetail string
		}{
			{domain.RefundSucceeded, "CN-PART · $10.00 refunded to card · $70.00 to credit balance"},
			{domain.RefundFailed, "CN-PART · $10.00 to card · $70.00 to credit balance"},
			{domain.RefundUnknown, "CN-PART · $10.00 to card · $70.00 to credit balance"},
		} {
			resp := seed(t, domain.CreditNote{
				CreditNoteNumber: "CN-PART", TotalCents: 8000, Currency: "USD",
				RefundAmountCents: 1000, CreditAmountCents: 7000, RefundStatus: tc.status,
			})
			if got := rowFor(t, resp, "CN-PART"); got.desc != "Credit note issued" || got.detail != tc.wantDetail {
				t.Errorf("refund_status=%q rendered %q / %q, want Credit note issued / %q", tc.status, got.desc, got.detail, tc.wantDetail)
			}
		}
	})

	// A note with no card leg at all is unaffected by refund status.
	t.Run("no refund leg is a plain success", func(t *testing.T) {
		resp := seed(t, domain.CreditNote{
			CreditNoteNumber: "CN-CREDIT", TotalCents: 1000, Currency: "USD",
			CreditAmountCents: 1000, RefundStatus: domain.RefundNone,
		})
		if got := rowFor(t, resp, "CN-CREDIT"); got.desc != "Credit note issued" || got.status != "succeeded" || got.problem != "" {
			t.Errorf("credit-only note rendered %+v, want a plain success", got)
		}
	})
}
