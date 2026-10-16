package invoice

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// tzTermsSettings returns a tenant timezone and Net terms.
type tzTermsSettings struct{ tz string }

func (f tzTermsSettings) Get(_ context.Context, _ string) (domain.TenantSettings, error) {
	return domain.TenantSettings{Timezone: f.tz, NetPaymentTerms: 30}, nil
}

// A manual invoice carries the org billing timezone, as every engine invoice
// does. Pre-fix it was empty, so the PDF rendered Issued/Due/Paid/Void in UTC:
// an America/Los_Angeles tenant finalizing at 2026-10-16 18:00 PDT
// (2026-10-17T01:00Z) saw "Oct 16" on the dashboard and "Issued October 17,
// 2026" on the PDF.
func TestManualInvoice_StampsTenantBillingTimezone(t *testing.T) {
	at := time.Date(2026, 10, 17, 1, 0, 0, 0, time.UTC) // Oct 16 18:00 PDT
	svc := NewService(newMemStore(), clock.NewFake(at), newMemNumberer())
	svc.SetTenantSettingsReader(tzTermsSettings{tz: "America/Los_Angeles"})
	ctx := context.Background()

	inv, err := svc.Create(ctx, "t1", CreateInput{
		CustomerID:         "cus_1",
		NetPaymentTermDays: intPtr(7),
		LineItems:          []AddLineItemInput{{Description: "Consulting", Quantity: 1, UnitAmountCents: 10000}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if inv.BillingTimezone != "America/Los_Angeles" {
		t.Fatalf("create billing_timezone = %q, want %q", inv.BillingTimezone, "America/Los_Angeles")
	}

	final, err := svc.Finalize(ctx, "t1", inv.ID)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if final.BillingTimezone != "America/Los_Angeles" {
		t.Fatalf("finalized billing_timezone = %q, want %q", final.BillingTimezone, "America/Los_Angeles")
	}
	// The PDF's document zone is LoadLocationOrUTC(inv.BillingTimezone)
	// (pdf.go); the Issued row must read the tenant's civil day.
	docLoc := domain.LoadLocationOrUTC(final.BillingTimezone)
	if got := final.IssuedAt.In(docLoc).Format("January 2, 2006"); got != "October 16, 2026" {
		t.Errorf("PDF Issued date = %q, want %q", got, "October 16, 2026")
	}
}

// With no timezone configured the stamp is "UTC", matching the engine.
func TestManualInvoice_NoTenantTimezoneStampsUTC(t *testing.T) {
	svc := NewService(newMemStore(), nil, newMemNumberer())
	svc.SetTenantSettingsReader(tzTermsSettings{})
	inv, err := svc.Create(context.Background(), "t1", CreateInput{CustomerID: "cus_1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if inv.BillingTimezone != "UTC" {
		t.Errorf("billing_timezone = %q, want %q", inv.BillingTimezone, "UTC")
	}
}

// An explicit Net term no longer skips the settings read, so an unreadable
// settings row now fails the create too (it needs the timezone), before an
// invoice number is spent.
func TestManualInvoice_UnreadableSettingsFailsCreateWithExplicitTerms(t *testing.T) {
	numberer := newMemNumberer()
	svc := NewService(newMemStore(), nil, numberer)
	svc.SetTenantSettingsReader(fakeSettingsReader{err: errors.New("settings read: connection reset")})
	if _, err := svc.Create(context.Background(), "t1", CreateInput{CustomerID: "cus_1", NetPaymentTermDays: intPtr(14)}); err == nil {
		t.Fatal("create succeeded with unreadable tenant settings; want an error")
	}
	if numberer.next != 0 {
		t.Errorf("an invoice number was spent before the failure (%d allocated)", numberer.next)
	}
}
