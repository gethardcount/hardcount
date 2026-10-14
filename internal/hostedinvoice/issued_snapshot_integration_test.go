package hostedinvoice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/crypto"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestHostedView_IssuedInvoiceKeepsIssueTimeParties (ADR-109, #964): the
// hosted page is the copy a customer opens from the invoice email. After the
// operator edits the billing profile and the company details, the page for an
// already-issued invoice must still show the details it was issued with.
// Branding (brand color) is not part of the issued document and stays live:
// that assertion is the control that proves the settings edit landed.
func TestHostedView_IssuedInvoiceKeepsIssueTimeParties(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Hosted Snapshot Corp")
	customers := customer.NewPostgresStore(db)
	settings := tenant.NewSettingsStore(db)
	invoices := invoice.NewPostgresStore(db)
	// Encryption on, as in production (router.go).
	enc, err := crypto.NewEncryptor(strings.Repeat("ef", 32))
	if err != nil {
		t.Fatalf("encryptor: %v", err)
	}
	customers.SetEncryptor(enc)
	invoices.SetEncryptor(enc)

	cust, err := customers.Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_hosted_snap", DisplayName: "Hosted Buyer", Email: "ap@hosted.test",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	setParties := func(legal, line1, country, company, companyLine1, brand string) {
		t.Helper()
		if _, err := customers.UpsertBillingProfile(ctx, tenantID, domain.CustomerBillingProfile{
			CustomerID: cust.ID, LegalName: legal, AddressLine1: line1, City: "City of " + legal,
			Country: country, TaxStatus: domain.TaxStatusStandard, ProfileStatus: domain.BillingProfileReady,
		}); err != nil {
			t.Fatalf("upsert profile: %v", err)
		}
		ts, err := settings.Get(ctx, tenantID)
		if err != nil {
			t.Fatalf("get settings: %v", err)
		}
		ts.CompanyName, ts.CompanyAddressLine1, ts.BrandColor = company, companyLine1, brand
		if _, err := settings.Upsert(ctx, ts); err != nil {
			t.Fatalf("upsert settings: %v", err)
		}
	}
	setParties("Before GmbH", "Alte Strasse 1", "DE", "Seller Before Ltd", "1 Old Road", "#111111")

	now := time.Now().UTC()
	inv, err := invoices.CreateWithLineItems(ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "HOST-SNAP-1", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentPending, Currency: "EUR",
		SubtotalCents: 1000, TotalAmountCents: 1000, AmountDueCents: 1000,
		BillingPeriodStart: now.Add(-24 * time.Hour), BillingPeriodEnd: now, IssuedAt: &now,
		TaxFacts:      domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK},
		BillingReason: domain.BillingReasonSubscriptionCycle,
	}, nil)
	if err != nil {
		t.Fatalf("create finalized invoice: %v", err)
	}
	if inv.PublicToken == "" {
		t.Fatal("finalized invoice has no public token")
	}

	setParties("After AG", "Neue Strasse 9", "AT", "Seller After Inc", "2 New Avenue", "#222222")

	// Production wires the invoice service here (router.go).
	h := New(Deps{Invoices: invoice.NewService(invoices, nil, settings), Customers: customers, Settings: settings})
	w := httptest.NewRecorder()
	mountRouter(h).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/"+inv.PublicToken, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET hosted view: %d %s", w.Code, w.Body.String())
	}
	var got hostedInvoicePayload
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	checks := []struct{ field, got, want string }{
		{"bill_to.name", got.BillTo.Name, "Before GmbH"},
		{"bill_to.address_line1", got.BillTo.AddressLine1, "Alte Strasse 1"},
		{"bill_to.city", got.BillTo.City, "City of Before GmbH"},
		{"bill_to.country", got.BillTo.Country, "DE"},
		{"bill_to.email", got.BillTo.Email, "ap@hosted.test"},
		{"branding.company_name", got.Branding.CompanyName, "Seller Before Ltd"},
		{"branding.company_address_line1", got.Branding.CompanyAddressLine1, "1 Old Road"},
		{"branding.brand_color (stays live)", got.Branding.BrandColor, "#222222"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("hosted view %s = %q, want %q", c.field, c.got, c.want)
		}
	}
}
