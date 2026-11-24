package invoice

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// A request reaches an invoice handler on the request's instant;
// atDecidedTime stands for it (entryDecidedAt). These pin that the work a
// handler starts runs on it: the collect a finalize starts, the setup-link
// and invoice emails (the email outbox records the instant a send's ctx
// carries, email.OutboxStore.Enqueue), and the audit rows, here a no-charge
// settle's and a refund's.

func instantOn(ctx context.Context) time.Time {
	at, _ := clock.EffectiveNow(ctx)
	return at
}

func requestAtDecidedTime(method, path, id string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, path, body)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	ctx := context.WithValue(auth.WithTenantID(atDecidedTime(), "t1"), chi.RouteCtxKey, rctx)
	return req.WithContext(ctx)
}

type simCollector struct {
	calls int
	saw   time.Time
}

func (c *simCollector) CollectInvoice(ctx context.Context, _, _ string, _ *time.Time) {
	c.calls++
	c.saw = instantOn(ctx)
}

type simNotifier struct{ saw time.Time }

func (n *simNotifier) NotifyNoPaymentMethod(ctx context.Context, _ string, _ domain.Invoice, _ string) (domain.NotifyOutcome, error) {
	n.saw = instantOn(ctx)
	return domain.NotifySent, nil
}

type simSender struct {
	ccCapturingSender
	saw time.Time
}

func (s *simSender) SendInvoice(ctx context.Context, tenantID, to string, cc []string, a, b string, amount int64, c string, pdf []byte, d string, writtenOff bool) error {
	s.saw = instantOn(ctx)
	return s.ccCapturingSender.SendInvoice(ctx, tenantID, to, cc, a, b, amount, c, pdf, d, writtenOff)
}

// simAudit records the instant each audit row's ctx carries, by action.
type simAudit struct{ rows map[string]time.Time }

func (a *simAudit) Log(ctx context.Context, _, action, _, _, _ string, _ map[string]any) error {
	a.rows[action] = instantOn(ctx)
	return nil
}

type cnRefundIssuer struct{}

func (cnRefundIssuer) IssueRefund(_ context.Context, _ string, in RefundInput) (domain.CreditNote, error) {
	return domain.CreditNote{ID: "hc_cn_1", InvoiceID: in.InvoiceID, CreditNoteNumber: "CN-1", RefundAmountCents: in.AmountCents}, nil
}

func TestHandlers_WorkRunsOnTheRequestsTime(t *testing.T) {
	seed := func(t *testing.T, store *memStore, inv domain.Invoice) domain.Invoice {
		t.Helper()
		inv.CustomerID, inv.Currency = "cus_1", "USD"
		created, err := store.Create(context.Background(), "t1", inv)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		return created
	}

	t.Run("the collect a finalize starts runs on the request's time", func(t *testing.T) {
		store := newMemStore()
		svc := NewService(store, clock.Real(), newMemNumberer())
		draft, err := svc.Create(atDecidedTime(), "t1", CreateInput{
			CustomerID: "cus_1",
			LineItems:  []AddLineItemInput{{Description: "Consulting", Quantity: 1, UnitAmountCents: 10000}},
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		col := &simCollector{}
		h := &Handler{svc: svc, collector: col}
		rr := httptest.NewRecorder()
		h.finalize(rr, requestAtDecidedTime(http.MethodPost, "/v1/invoices/"+draft.ID+"/finalize", draft.ID, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("finalize: %d %s", rr.Code, rr.Body.String())
		}
		if col.calls != 1 || !col.saw.Equal(entryDecidedAt) {
			t.Errorf("collect ran %d times on %+v; want once on %+v", col.calls, col.saw, entryDecidedAt)
		}
	})

	t.Run("a resent setup link's email carries the request's time", func(t *testing.T) {
		store := newMemStore()
		inv := seed(t, store, domain.Invoice{Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed, AmountDueCents: 5000})
		n := &simNotifier{}
		h := &Handler{svc: NewService(store, clock.Real(), newMemNumberer()), noPMNotifier: n, emailConfigured: true}
		rr := httptest.NewRecorder()
		h.resendSetupLink(rr, requestAtDecidedTime(http.MethodPost, "/v1/invoices/"+inv.ID+"/resend-setup-link", inv.ID, nil))
		if rr.Code/100 != 2 {
			t.Fatalf("resend: %d %s", rr.Code, rr.Body.String())
		}
		if !n.saw.Equal(entryDecidedAt) {
			t.Errorf("setup-link email on %+v; want %+v", n.saw, entryDecidedAt)
		}
	})

	t.Run("an invoice email carries its caller's time", func(t *testing.T) {
		h, _, _, inv := emailerFixture(t, domain.InvoiceFinalized, domain.Customer{ID: "cus_auto", Email: "ap@auto.test"})
		sender := &simSender{}
		h.emailSender = sender
		if err := h.emailInvoice(atDecidedTime(), "t1", inv, nil, "ap@auto.test", nil, ""); err != nil {
			t.Fatalf("emailInvoice: %v", err)
		}
		if !sender.saw.Equal(entryDecidedAt) {
			t.Errorf("invoice email on %+v; want %+v", sender.saw, entryDecidedAt)
		}
	})

	t.Run("a no-charge settle's audit row is on the request's time", func(t *testing.T) {
		store := newMemStore()
		inv := seed(t, store, domain.Invoice{InvoiceNumber: "INV-0", Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending})
		aud := &simAudit{rows: map[string]time.Time{}}
		h := &Handler{svc: NewService(store, clock.Real(), newMemNumberer()), charger: &fakeCharger{}, auditLogger: aud,
			paymentSetups: &fakePaymentSetups{setup: domain.CustomerPaymentSetup{SetupStatus: domain.PaymentSetupReady, StripeCustomerID: "cus_stripe_1"}}}
		rr := httptest.NewRecorder()
		h.collectPayment(rr, requestAtDecidedTime(http.MethodPost, "/v1/invoices/"+inv.ID+"/collect", inv.ID, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("collect: %d %s", rr.Code, rr.Body.String())
		}
		if got, ok := aud.rows[domain.AuditActionCollect]; !ok || !got.Equal(entryDecidedAt) {
			t.Errorf("collect audit row on %+v (written %v); want %+v", got, ok, entryDecidedAt)
		}
	})

	t.Run("a refund's audit row is on the request's time", func(t *testing.T) {
		store := newMemStore()
		inv := seed(t, store, domain.Invoice{InvoiceNumber: "INV-R", Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded, TotalAmountCents: 5000})
		aud := &simAudit{rows: map[string]time.Time{}}
		h := &Handler{svc: NewService(store, clock.Real(), newMemNumberer()), refundIssuer: cnRefundIssuer{}, auditLogger: aud}
		rr := httptest.NewRecorder()
		h.refund(rr, requestAtDecidedTime(http.MethodPost, "/v1/invoices/"+inv.ID+"/refund", inv.ID,
			strings.NewReader(`{"amount_cents": 1000, "reason": "requested_by_customer"}`)))
		if rr.Code/100 != 2 {
			t.Fatalf("refund: %d %s", rr.Code, rr.Body.String())
		}
		if got, ok := aud.rows[domain.AuditActionRefund]; !ok || !got.Equal(entryDecidedAt) {
			t.Errorf("refund audit row on %+v (written %v); want %+v", got, ok, entryDecidedAt)
		}
	})
}
