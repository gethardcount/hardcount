package dunning

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
)

type recordingVoider struct {
	calls       int
	recordCalls int
	recordNotes []string
	err         error
}

func (v *recordingVoider) RecordOfflinePayment(_ context.Context, tenantID, id, note string) (domain.Invoice, error) {
	v.recordCalls++
	v.recordNotes = append(v.recordNotes, note)
	if v.err != nil {
		return domain.Invoice{}, v.err
	}
	return domain.Invoice{ID: id, TenantID: tenantID, CustomerID: "cus_1", Status: domain.InvoicePaid, PaidOutOfBand: true}, nil
}

func (v *recordingVoider) Void(_ context.Context, tenantID, id string) (domain.Invoice, error) {
	v.calls++
	if v.err != nil {
		return domain.Invoice{}, v.err
	}
	return domain.Invoice{ID: id, TenantID: tenantID, CustomerID: "cus_1", Status: domain.InvoiceVoided, StripePaymentIntentID: "pi_1"}, nil
}

// TestResolveRun_RejectsUnknownResolution pins the operator-input contract
// (2026-07-26, found live on FLOW I5): the endpoint stored ANY string
// verbatim — a typo'd "payment_received" landed in
// invoice_dunning_runs.resolution, silently skipped the mark-paid
// propagation (the switch matched nothing), and rendered as a raw slug in
// the dashboard. Only the three operator-choosable outcomes are legal;
// engine-written states (retries_exhausted, action_failed) and free text
// are 422s.
func TestResolveRun_RejectsUnknownResolution(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newMemStore(), &noopRetrier{}, nil)
	run, err := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", time.Now(), domain.DunningCausePaymentFailed)
	if err != nil {
		t.Fatalf("StartDunning: %v", err)
	}
	h := NewHandler(svc)

	for _, bad := range []string{"payment_received", "retries_exhausted", "action_failed", "", "whatever"} {
		body, _ := json.Marshal(ResolveInput{Resolution: bad})
		r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", run.ID)
		r = r.WithContext(context.WithValue(auth.WithTenantID(r.Context(), "t1"), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		h.resolveRun(rec, r)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("resolution %q: got %d, want 422", bad, rec.Code)
		}
	}

	// The run must be untouched by the rejected attempts.
	got, err := svc.store.GetRun(ctx, "t1", run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.State != domain.DunningActive || got.Resolution != "" {
		t.Errorf("run mutated by rejected resolution: state=%s resolution=%q", got.State, got.Resolution)
	}
}

// TestResolveRun_PaymentRecovered_RoutesThroughOfflinePaymentWriter pins the
// single-offline-payment-writer contract (2026-07-26): payment_recovered
// resolves through invoice.Service.RecordOfflinePayment — the same path as
// the invoice page's Record-offline-payment dialog — so the recovery is
// recorded as paid out of band, with the in-flight guard and the
// invoice.payment_recorded webhook. Pre-fix it called a raw MarkPaid("")
// with none of those.
func TestResolveRun_PaymentRecovered_RoutesThroughOfflinePaymentWriter(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newMemStore(), &noopRetrier{}, nil)
	run, err := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", time.Now(), domain.DunningCausePaymentFailed)
	if err != nil {
		t.Fatalf("StartDunning: %v", err)
	}
	voider := &recordingVoider{}
	h := NewHandler(svc)
	h.SetInvoiceVoider(voider)

	body, _ := json.Marshal(ResolveInput{Resolution: string(domain.ResolutionPaymentRecovered)})
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", run.ID)
	r = r.WithContext(context.WithValue(auth.WithTenantID(r.Context(), "t1"), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.resolveRun(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("resolve: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if voider.recordCalls != 1 {
		t.Fatalf("RecordOfflinePayment calls = %d, want 1 (raw MarkPaid is the dual-writer hole)", voider.recordCalls)
	}
	if len(voider.recordNotes) != 1 || voider.recordNotes[0] != "Recovered via dunning resolution" {
		t.Errorf("note = %v, want the dunning-resolution memo", voider.recordNotes)
	}
	if voider.calls != 0 {
		t.Errorf("Void must not run on payment_recovered; calls=%d", voider.calls)
	}
}
