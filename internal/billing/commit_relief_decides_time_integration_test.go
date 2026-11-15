package billing_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// onceThenUnreadable answers the first invoice lookup with clock clk_1 at
// frozen and fails every later one.
type onceThenUnreadable struct {
	frozen time.Time
	reads  int
}

func (o *onceThenUnreadable) SimForInvoice(context.Context, string, string) (clock.Sim, error) {
	o.reads++
	if o.reads == 1 {
		return clock.Sim{At: o.frozen, TestClockID: "clk_1"}, nil
	}
	return clock.Sim{}, errors.New("connection reset by peer")
}
func (o *onceThenUnreadable) SimForSubscription(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{}, errors.New("not used")
}
func (o *onceThenUnreadable) SimForCustomer(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{}, errors.New("not used")
}

// noClockAnswer puts every record on no test clock (the engine's answer for
// one), and counts the reads.
type noClockAnswer struct{ reads int }

func (n *noClockAnswer) SimForInvoice(context.Context, string, string) (clock.Sim, error) {
	n.reads++
	return clock.Sim{}, nil
}
func (n *noClockAnswer) SimForSubscription(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{}, errors.New("not used")
}
func (n *noClockAnswer) SimForCustomer(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{}, errors.New("not used")
}

// reliefThroughTheRoute pays a commit invoice and sends its commit relief
// through the credit note route on svc's real stores, and returns the note.
func reliefThroughTheRoute(t *testing.T, name string, prepare func(svc *creditnote.Service)) domain.CreditNote {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, name)
	settings := tenant.NewSettingsStore(db)
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_" + tenantID, DisplayName: name})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	creditSvc := credit.NewService(credit.NewPostgresStore(db))
	invStore := invoice.NewPostgresStore(db)
	invStore.SetCommitFunder(creditSvc)
	invSvc := invoice.NewService(invStore, clock.Real(), settings)
	draft := createCommitInvoice(t, ctx, invSvc, tenantID, cust.ID, 10000, 10000)
	if _, err := invSvc.Finalize(ctx, tenantID, draft.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if _, err := invStore.MarkPaidOffline(ctx, tenantID, draft.ID, time.Now().UTC(), nil); err != nil {
		t.Fatalf("pay: %v", err)
	}
	svc := newReliefService(db, invStore, creditSvc, settings)
	prepare(svc)
	body := `{"invoice_id":"` + draft.ID + `","reason":"Lost card dispute","commit_relief":{"retire_all":true}}`
	req := httptest.NewRequest("POST", "/", strings.NewReader(body))
	req = req.WithContext(auth.WithTenantID(ctx, tenantID))
	rec := httptest.NewRecorder()
	creditnote.NewHandler(svc, creditnote.HandlerDeps{}).Routes().ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("relief: %d %s; want 201", rec.Code, rec.Body.String())
	}
	var cn domain.CreditNote
	if err := json.Unmarshal(rec.Body.Bytes(), &cn); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return cn
}

// On the real stores, through the credit note route: a commit relief runs on
// the route's decision, the invoice's clock at its frozen time. Nothing in the
// relief looks the clock up again (the resolver would fail any second read).
func TestCommitRelief_TheRoutesDecisionStands(t *testing.T) {
	frozen := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	res := &onceThenUnreadable{frozen: frozen}
	cn := reliefThroughTheRoute(t, "Commit Relief Decided", func(svc *creditnote.Service) { svc.SetResolver(res) })
	if !cn.CreatedAt.Equal(frozen) || cn.IssuedAt == nil || !cn.IssuedAt.Equal(frozen) {
		t.Errorf("created_at %v, issued_at %v; want both at the decided clock's %v", cn.CreatedAt, cn.IssuedAt, frozen)
	}
	if res.reads != 1 {
		t.Errorf("%d lookups; want only the route's", res.reads)
	}
}

// For an invoice on no test clock the route decides real time and binds no
// instant: the relief reads the server's clock once (here a fixed past
// instant, not the machine's), and its note is created and issued at it.
func TestCommitRelief_AtRealTimeIsOnTheServersClock(t *testing.T) {
	serverAt := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	cn := reliefThroughTheRoute(t, "Commit Relief Real Time", func(svc *creditnote.Service) {
		svc.SetResolver(&noClockAnswer{})
		svc.SetClock(clock.NewFake(serverAt))
	})
	if !cn.CreatedAt.Equal(serverAt) || cn.IssuedAt == nil || !cn.IssuedAt.Equal(serverAt) {
		t.Errorf("created_at %v, issued_at %v; want both at the server's %v", cn.CreatedAt, cn.IssuedAt, serverAt)
	}
}
