package invoice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
)

type fakeRefundIssuer struct {
	called bool
	last   RefundInput
	cn     domain.CreditNote
	err    error
}

func (f *fakeRefundIssuer) IssueRefund(_ context.Context, _ string, input RefundInput) (domain.CreditNote, error) {
	f.called = true
	f.last = input
	if f.err != nil {
		return domain.CreditNote{}, f.err
	}
	return f.cn, nil
}

func newRefundTestHandler(issuer RefundIssuer) *Handler {
	return &Handler{refundIssuer: issuer}
}

func postRefund(t *testing.T, h *Handler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/invoices/inv_1/refund", &buf)
	// Tenant + chi URL param setup that the middleware stack would normally do.
	ctx := context.WithValue(req.Context(), auth.TestTenantIDKey(), "t1")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "inv_1")
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	h.refund(rec, req)
	return rec
}

func TestRefundHandler_Success(t *testing.T) {
	t.Parallel()
	issuer := &fakeRefundIssuer{
		cn: domain.CreditNote{
			ID: "cn_1", CreditNoteNumber: "CN-2026-04-000001",
			RefundAmountCents: 5000, Currency: "USD",
			Status: domain.CreditNoteIssued, RefundStatus: domain.RefundSucceeded,
		},
	}
	h := newRefundTestHandler(issuer)

	rec := postRefund(t, h, map[string]any{
		"amount_cents": 5000,
		"reason":       "requested_by_customer",
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 — body=%s", rec.Code, rec.Body.String())
	}
	if !issuer.called {
		t.Fatal("expected IssueRefund to be called")
	}
	if issuer.last.InvoiceID != "inv_1" {
		t.Errorf("invoice_id: got %q, want inv_1", issuer.last.InvoiceID)
	}
	if issuer.last.AmountCents != 5000 {
		t.Errorf("amount_cents: got %d, want 5000", issuer.last.AmountCents)
	}
	if issuer.last.Reason != "requested_by_customer" {
		t.Errorf("reason: got %q", issuer.last.Reason)
	}
}

func TestRefundHandler_DefaultAmountPassthrough(t *testing.T) {
	t.Parallel()
	issuer := &fakeRefundIssuer{
		cn: domain.CreditNote{ID: "cn_1", Status: domain.CreditNoteIssued},
	}
	h := newRefundTestHandler(issuer)

	rec := postRefund(t, h, map[string]any{"reason": "duplicate"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if issuer.last.AmountCents != 0 {
		t.Errorf("amount_cents: got %d, want 0 (defaults to full refundable)", issuer.last.AmountCents)
	}
}

// A refund with no amount_cents refunds everything still refundable, so a
// misspelled amount key (Stripe's `amount`) was a 201 full card refund. The
// unknown key is now a 422 naming it, and nothing is refunded.
func TestRefundHandler_RefusesUnknownField(t *testing.T) {
	t.Parallel()
	issuer := &fakeRefundIssuer{}
	h := newRefundTestHandler(issuer)

	rec := postRefund(t, h, map[string]any{"amount": 500, "reason": "requested_by_customer"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422 — body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct {
			Param string `json:"param"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Param != "amount" {
		t.Errorf("param: got %q, want amount", env.Error.Param)
	}
	if issuer.called {
		t.Error("IssueRefund must not be called for an unknown field")
	}
}

func TestRefundHandler_RejectsUnknownReason(t *testing.T) {
	t.Parallel()
	issuer := &fakeRefundIssuer{}
	h := newRefundTestHandler(issuer)

	rec := postRefund(t, h, map[string]any{"reason": "vibes_off"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422", rec.Code)
	}
	if issuer.called {
		t.Error("IssueRefund should not be called on invalid reason")
	}
}

func TestRefundHandler_AllValidReasons(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{"duplicate", "fraudulent", "requested_by_customer", "other"} {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			issuer := &fakeRefundIssuer{cn: domain.CreditNote{Status: domain.CreditNoteIssued}}
			h := newRefundTestHandler(issuer)
			rec := postRefund(t, h, map[string]any{"reason": reason})
			if rec.Code != http.StatusOK {
				t.Errorf("reason %q: got %d, want 200 — body=%s", reason, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestRefundHandler_MissingReason(t *testing.T) {
	t.Parallel()
	issuer := &fakeRefundIssuer{}
	h := newRefundTestHandler(issuer)

	rec := postRefund(t, h, map[string]any{"amount_cents": 1000})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422 — body=%s", rec.Code, rec.Body.String())
	}
}

func TestRefundHandler_NegativeAmount(t *testing.T) {
	t.Parallel()
	issuer := &fakeRefundIssuer{}
	h := newRefundTestHandler(issuer)

	rec := postRefund(t, h, map[string]any{"amount_cents": -100, "reason": "other"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422", rec.Code)
	}
}

func TestRefundHandler_NotFound(t *testing.T) {
	t.Parallel()
	issuer := &fakeRefundIssuer{err: errs.ErrNotFound}
	h := newRefundTestHandler(issuer)

	rec := postRefund(t, h, map[string]any{"reason": "other"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
}

func TestRefundHandler_ServiceErrorBubblesUp(t *testing.T) {
	t.Parallel()
	issuer := &fakeRefundIssuer{err: errs.InvalidState("can only refund paid invoices")}
	h := newRefundTestHandler(issuer)

	rec := postRefund(t, h, map[string]any{"reason": "other"})
	// State conflicts (errs.InvalidState) map to HTTP 409, not 422.
	// 422 means "fix your input"; 409 means "the resource is in a
	// state that doesn't permit this operation" — the latter is
	// what "can only refund paid invoices" is. Stripe / GitHub /
	// AWS all follow this convention.
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409 — body=%s", rec.Code, rec.Body.String())
	}
}

func TestRefundHandler_ProviderNotConfigured(t *testing.T) {
	t.Parallel()
	h := &Handler{refundIssuer: nil}

	rec := postRefund(t, h, map[string]any{"reason": "other"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422", rec.Code)
	}
}

func TestRefundHandler_GenericError(t *testing.T) {
	t.Parallel()
	issuer := &fakeRefundIssuer{err: errors.New("boom")}
	h := newRefundTestHandler(issuer)

	rec := postRefund(t, h, map[string]any{"reason": "other"})
	// FromError falls back to internal error (500) for unclassified errors.
	if rec.Code == http.StatusOK {
		t.Fatalf("expected non-2xx; got %d", rec.Code)
	}
}
