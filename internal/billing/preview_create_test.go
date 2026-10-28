package billing

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/subscription"
)

// stubCustomers is a CustomerLookup that returns a canned customer for
// known IDs and ErrNotFound otherwise. Tests assert RLS-by-construction
// by setting unknown IDs.
type stubCustomers struct {
	known map[string]domain.Customer
}

func (s *stubCustomers) Get(_ context.Context, _, id string) (domain.Customer, error) {
	if c, ok := s.known[id]; ok {
		return c, nil
	}
	return domain.Customer{}, errs.ErrNotFound
}

// stubSubscriptions implements SubscriptionLister so tests can control
// the resolveSubscription branch (explicit ID happy path, mismatched
// customer, implicit pick of latest active sub, zero-active-subs error).
type stubSubscriptions struct {
	byID         map[string]domain.Subscription
	byCustomerID map[string][]domain.Subscription
	listErr      error
	getErr       error
}

func (s *stubSubscriptions) Get(_ context.Context, _, id string) (domain.Subscription, error) {
	if s.getErr != nil {
		return domain.Subscription{}, s.getErr
	}
	if sub, ok := s.byID[id]; ok {
		return sub, nil
	}
	return domain.Subscription{}, errs.ErrNotFound
}

func (s *stubSubscriptions) List(_ context.Context, filter subscription.ListFilter) ([]domain.Subscription, int, error) {
	if s.listErr != nil {
		return nil, 0, s.listErr
	}
	subs := s.byCustomerID[filter.CustomerID]
	return subs, len(subs), nil
}

// timePtr returns a pointer to its argument — handy for setting
// CurrentBillingPeriodStart / CurrentBillingPeriodEnd on test fixtures
// without local variables.
func timePtr(t time.Time) *time.Time { return &t }

func TestResolveCreatePreviewPeriod(t *testing.T) {
	cycleStart := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	cycleEnd := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	subWithCycle := domain.Subscription{
		CurrentBillingPeriodStart: timePtr(cycleStart),
		CurrentBillingPeriodEnd:   timePtr(cycleEnd),
	}

	cases := []struct {
		name      string
		period    CreatePreviewPeriod
		sub       domain.Subscription
		wantFrom  time.Time
		wantTo    time.Time
		wantErr   bool
		wantField string
	}{
		{
			name:     "default to current cycle",
			period:   CreatePreviewPeriod{},
			sub:      subWithCycle,
			wantFrom: cycleStart,
			wantTo:   cycleEnd,
		},
		{
			name: "explicit window honored",
			period: CreatePreviewPeriod{
				From: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
				To:   time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
			},
			sub:      subWithCycle,
			wantFrom: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
			wantTo:   time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "partial bounds rejected (only from)",
			period: CreatePreviewPeriod{
				From: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
			},
			sub:       subWithCycle,
			wantErr:   true,
			wantField: "period",
		},
		{
			name: "partial bounds rejected (only to)",
			period: CreatePreviewPeriod{
				To: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
			},
			sub:       subWithCycle,
			wantErr:   true,
			wantField: "period",
		},
		{
			name: "from >= to rejected",
			period: CreatePreviewPeriod{
				From: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
				To:   time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
			},
			sub:       subWithCycle,
			wantErr:   true,
			wantField: "period",
		},
		{
			name:      "default with no current cycle is coded error",
			period:    CreatePreviewPeriod{},
			sub:       domain.Subscription{},
			wantErr:   true,
			wantField: "period",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from, to, err := resolveCreatePreviewPeriod(tc.period, tc.sub)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got from=%v to=%v", from, to)
				}
				if errs.Field(err) != tc.wantField {
					t.Errorf("field mismatch: got %q want %q", errs.Field(err), tc.wantField)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !from.Equal(tc.wantFrom) {
				t.Errorf("from: got %v want %v", from, tc.wantFrom)
			}
			if !to.Equal(tc.wantTo) {
				t.Errorf("to: got %v want %v", to, tc.wantTo)
			}
		})
	}
}

func TestPickPrimarySubscription(t *testing.T) {
	earlyStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	earlyEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	lateStart := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	lateEnd := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name   string
		subs   []domain.Subscription
		wantOk bool
		wantID string
	}{
		{
			name:   "empty input returns false",
			subs:   nil,
			wantOk: false,
		},
		{
			name: "only canceled sub returns false",
			subs: []domain.Subscription{
				{ID: "s1", Status: domain.SubscriptionCanceled, CurrentBillingPeriodStart: timePtr(lateStart), CurrentBillingPeriodEnd: timePtr(lateEnd)},
			},
			wantOk: false,
		},
		{
			name: "active sub without cycle is skipped",
			subs: []domain.Subscription{
				{ID: "s1", Status: domain.SubscriptionActive},
			},
			wantOk: false,
		},
		{
			name: "single active sub picked",
			subs: []domain.Subscription{
				{ID: "s1", Status: domain.SubscriptionActive, CurrentBillingPeriodStart: timePtr(lateStart), CurrentBillingPeriodEnd: timePtr(lateEnd)},
			},
			wantOk: true,
			wantID: "s1",
		},
		{
			name: "trialing sub picked",
			subs: []domain.Subscription{
				{ID: "s1", Status: domain.SubscriptionTrialing, CurrentBillingPeriodStart: timePtr(lateStart), CurrentBillingPeriodEnd: timePtr(lateEnd)},
			},
			wantOk: true,
			wantID: "s1",
		},
		{
			name: "most recent cycle wins on tie",
			subs: []domain.Subscription{
				{ID: "s1", Status: domain.SubscriptionActive, CurrentBillingPeriodStart: timePtr(earlyStart), CurrentBillingPeriodEnd: timePtr(earlyEnd)},
				{ID: "s2", Status: domain.SubscriptionActive, CurrentBillingPeriodStart: timePtr(lateStart), CurrentBillingPeriodEnd: timePtr(lateEnd)},
			},
			wantOk: true,
			wantID: "s2",
		},
		{
			name: "canceled siblings ignored",
			subs: []domain.Subscription{
				{ID: "s1", Status: domain.SubscriptionCanceled, CurrentBillingPeriodStart: timePtr(lateStart), CurrentBillingPeriodEnd: timePtr(lateEnd)},
				{ID: "s2", Status: domain.SubscriptionActive, CurrentBillingPeriodStart: timePtr(earlyStart), CurrentBillingPeriodEnd: timePtr(earlyEnd)},
			},
			wantOk: true,
			wantID: "s2",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := pickPrimarySubscription(tc.subs)
			if ok != tc.wantOk {
				t.Fatalf("ok: got %v want %v", ok, tc.wantOk)
			}
			if !ok {
				return
			}
			if got.ID != tc.wantID {
				t.Errorf("ID: got %q want %q", got.ID, tc.wantID)
			}
		})
	}
}

func TestPreviewService_ResolveSubscription(t *testing.T) {
	subActive := domain.Subscription{
		ID:                        "hc_sub_active",
		CustomerID:                "hc_cus_abc",
		Status:                    domain.SubscriptionActive,
		CurrentBillingPeriodStart: timePtr(time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)),
		CurrentBillingPeriodEnd:   timePtr(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)),
	}
	subForOtherCustomer := domain.Subscription{
		ID:         "hc_sub_active",
		CustomerID: "hc_cus_other",
		Status:     domain.SubscriptionActive,
	}

	t.Run("explicit ID happy path", func(t *testing.T) {
		svc := &PreviewService{
			subscriptions: &stubSubscriptions{
				byID: map[string]domain.Subscription{"hc_sub_active": subActive},
			},
		}
		sub, err := svc.resolveSubscription(context.Background(), "t1", "hc_cus_abc", "hc_sub_active")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if sub.ID != "hc_sub_active" {
			t.Errorf("ID: got %q", sub.ID)
		}
	})

	t.Run("explicit ID belongs to wrong customer", func(t *testing.T) {
		svc := &PreviewService{
			subscriptions: &stubSubscriptions{
				byID: map[string]domain.Subscription{"hc_sub_active": subForOtherCustomer},
			},
		}
		_, err := svc.resolveSubscription(context.Background(), "t1", "hc_cus_abc", "hc_sub_active")
		if err == nil {
			t.Fatal("expected validation error, got nil")
		}
		if errs.Field(err) != "subscription_id" {
			t.Errorf("field: got %q want subscription_id", errs.Field(err))
		}
	})

	t.Run("explicit ID 404 propagates", func(t *testing.T) {
		svc := &PreviewService{
			subscriptions: &stubSubscriptions{
				byID: map[string]domain.Subscription{},
			},
		}
		_, err := svc.resolveSubscription(context.Background(), "t1", "hc_cus_abc", "hc_sub_missing")
		if err == nil {
			t.Fatal("expected error")
		}
		if !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("implicit picks most recent active sub", func(t *testing.T) {
		earlyStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
		earlyEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
		lateStart := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
		lateEnd := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
		svc := &PreviewService{
			subscriptions: &stubSubscriptions{
				byCustomerID: map[string][]domain.Subscription{
					"hc_cus_abc": {
						{ID: "old", CustomerID: "hc_cus_abc", Status: domain.SubscriptionActive, CurrentBillingPeriodStart: timePtr(earlyStart), CurrentBillingPeriodEnd: timePtr(earlyEnd)},
						{ID: "new", CustomerID: "hc_cus_abc", Status: domain.SubscriptionActive, CurrentBillingPeriodStart: timePtr(lateStart), CurrentBillingPeriodEnd: timePtr(lateEnd)},
					},
				},
			},
		}
		sub, err := svc.resolveSubscription(context.Background(), "t1", "hc_cus_abc", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if sub.ID != "new" {
			t.Errorf("expected most recent sub 'new', got %q", sub.ID)
		}
	})

	t.Run("implicit with zero subs returns coded error", func(t *testing.T) {
		svc := &PreviewService{
			subscriptions: &stubSubscriptions{
				byCustomerID: map[string][]domain.Subscription{},
			},
		}
		_, err := svc.resolveSubscription(context.Background(), "t1", "hc_cus_abc", "")
		if err == nil {
			t.Fatal("expected error")
		}
		if errs.Code(err) != "customer_has_no_subscription" {
			t.Errorf("code: got %q want customer_has_no_subscription", errs.Code(err))
		}
	})
}

// TestCreatePreview_BlankCustomerID surfaces the customer_id required
// error before any store calls — keeps the error shape symmetric with
// the customer-usage surface (errs.Required("customer_id")).
func TestCreatePreview_BlankCustomerID(t *testing.T) {
	svc := &PreviewService{
		customers:     &stubCustomers{},
		subscriptions: &stubSubscriptions{},
	}
	_, err := svc.CreatePreview(context.Background(), "t1", CreatePreviewRequest{CustomerID: "  "})
	if err == nil {
		t.Fatal("expected error")
	}
	if errs.Field(err) != "customer_id" {
		t.Errorf("field: got %q want customer_id", errs.Field(err))
	}
}

// TestCreatePreview_CustomerNotFound is the RLS-by-construction case:
// cross-tenant IDs return ErrNotFound at the customer lookup, which the
// handler maps to 404. No subscription / pricing reads happen.
func TestCreatePreview_CustomerNotFound(t *testing.T) {
	svc := &PreviewService{
		customers:     &stubCustomers{known: map[string]domain.Customer{}},
		subscriptions: &stubSubscriptions{},
	}
	_, err := svc.CreatePreview(context.Background(), "t1", CreatePreviewRequest{CustomerID: "hc_cus_other"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// TestDecodeCreatePreviewRequest covers the JSON wire-decoding edge
// cases the handler relies on: empty body is treated as {}, an unknown key
// is refused, partial period is rejected with field tagging, RFC 3339 must
// parse.
func TestDecodeCreatePreviewRequest(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr bool
		wantCID string
		wantSID string
	}{
		{name: "empty body", body: "", wantErr: false},
		{name: "minimal valid", body: `{"customer_id":"hc_cus_abc"}`, wantCID: "hc_cus_abc"},
		{name: "with subscription", body: `{"customer_id":"hc_cus_abc","subscription_id":"hc_sub_xyz"}`, wantCID: "hc_cus_abc", wantSID: "hc_sub_xyz"},
		{name: "with full period", body: `{"customer_id":"hc_cus_abc","period":{"from":"2026-04-01T00:00:00Z","to":"2026-05-01T00:00:00Z"}}`, wantCID: "hc_cus_abc"},
		{name: "malformed json", body: `{`, wantErr: true},
		{name: "unknown key", body: `{"customer_id":"hc_cus_abc","subscription":"hc_sub_xyz"}`, wantErr: true},
		{name: "unparseable from", body: `{"customer_id":"hc_cus_abc","period":{"from":"yesterday","to":"2026-05-01T00:00:00Z"}}`, wantErr: true},
		{name: "unparseable to", body: `{"customer_id":"hc_cus_abc","period":{"from":"2026-04-01T00:00:00Z","to":"tomorrow"}}`, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var wire CreatePreviewWireRequest
			err := respond.DecodeBody(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body)), &wire, respond.EmptyBodyOK)
			var req CreatePreviewRequest
			if err == nil {
				req, err = createPreviewRequestFromWire(wire)
			}
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", req)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if req.CustomerID != tc.wantCID {
				t.Errorf("customer_id: got %q want %q", req.CustomerID, tc.wantCID)
			}
			if req.SubscriptionID != tc.wantSID {
				t.Errorf("subscription_id: got %q want %q", req.SubscriptionID, tc.wantSID)
			}
		})
	}
}

// TestComputePreviewTotals exercises the per-currency rollup —
// insertion-stable order, single-currency one-entry, multi-currency
// distinct entries, blank-currency lines (e.g. zero-fee plans) excluded.
func TestComputePreviewTotals(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		totals := mustPreviewTotals(t, nil)
		if len(totals) != 0 {
			t.Errorf("expected empty totals, got %d", len(totals))
		}
	})

	t.Run("single currency", func(t *testing.T) {
		totals := mustPreviewTotals(t, []PreviewLine{
			{Currency: "USD", AmountCents: 100},
			{Currency: "USD", AmountCents: 250},
		})
		if len(totals) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(totals))
		}
		if totals[0].Currency != "USD" || totals[0].AmountCents != 350 {
			t.Errorf("got %+v", totals[0])
		}
	})

	t.Run("multi currency stable order", func(t *testing.T) {
		totals := mustPreviewTotals(t, []PreviewLine{
			{Currency: "USD", AmountCents: 100},
			{Currency: "EUR", AmountCents: 200},
			{Currency: "USD", AmountCents: 50},
		})
		if len(totals) != 2 {
			t.Fatalf("expected 2 entries, got %d", len(totals))
		}
		// USD seen first → position 0
		if totals[0].Currency != "USD" || totals[0].AmountCents != 150 {
			t.Errorf("totals[0]: got %+v", totals[0])
		}
		if totals[1].Currency != "EUR" || totals[1].AmountCents != 200 {
			t.Errorf("totals[1]: got %+v", totals[1])
		}
	})

	t.Run("blank-currency lines excluded", func(t *testing.T) {
		totals := mustPreviewTotals(t, []PreviewLine{
			{Currency: "", AmountCents: 999},
			{Currency: "USD", AmountCents: 100},
		})
		if len(totals) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(totals))
		}
		if totals[0].AmountCents != 100 {
			t.Errorf("expected 100, got %d", totals[0].AmountCents)
		}
	})
}

func mustPreviewTotals(t *testing.T, lines []PreviewLine) []PreviewTotal {
	t.Helper()
	totals, err := computePreviewTotals(lines)
	if err != nil {
		t.Fatalf("computePreviewTotals: %v", err)
	}
	return totals
}
