package creditnote

import (
	"context"
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

// TestCreditNotePDF_IssuedKeepsIssueTimeParties (ADR-109, #964): a credit note
// is an issued document like an invoice. Its bill-to and supplier blocks are
// taken when it is issued and do not follow a later profile or company edit.
// Both store entry points that issue a credit note are exercised; a draft
// credit note is the control and must follow the edit.
func TestCreditNotePDF_IssuedKeepsIssueTimeParties(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN Snapshot Corp")
	customers := customer.NewPostgresStore(db)
	settings := tenant.NewSettingsStore(db)
	invoices := invoice.NewPostgresStore(db)
	store := NewPostgresStore(db)
	// Encryption on, as in production (router.go): the customer PII the
	// issue stamp copies is stored encrypted.
	enc, err := crypto.NewEncryptor(strings.Repeat("cd", 32))
	if err != nil {
		t.Fatalf("encryptor: %v", err)
	}
	customers.SetEncryptor(enc)
	invoices.SetEncryptor(enc)
	store.SetEncryptor(enc)

	cust, err := customers.Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_cn_snap", DisplayName: "CN Buyer", Email: "ap@cn.test",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	setParties := func(legal, line1, country, taxID, company, companyLine1, companyTaxID string) {
		t.Helper()
		if _, err := customers.UpsertBillingProfile(ctx, tenantID, domain.CustomerBillingProfile{
			CustomerID: cust.ID, LegalName: legal, AddressLine1: line1, Country: country, TaxID: taxID,
			TaxStatus: domain.TaxStatusStandard, ProfileStatus: domain.BillingProfileReady,
		}); err != nil {
			t.Fatalf("upsert profile: %v", err)
		}
		ts, err := settings.Get(ctx, tenantID)
		if err != nil {
			t.Fatalf("get settings: %v", err)
		}
		ts.CompanyName, ts.CompanyAddressLine1, ts.TaxID = company, companyLine1, companyTaxID
		if _, err := settings.Upsert(ctx, ts); err != nil {
			t.Fatalf("upsert settings: %v", err)
		}
	}
	setParties("Before GmbH", "Alte Strasse 1", "DE", "DE111111111", "Seller Before Ltd", "1 Old Road", "IE1234567T")

	now := time.Now().UTC()
	inv, err := invoices.CreateWithLineItems(ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "CN-SNAP-INV-1", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentPending, Currency: "EUR",
		SubtotalCents: 9000, TotalAmountCents: 9000, AmountDueCents: 9000,
		BillingPeriodStart: now.Add(-24 * time.Hour), BillingPeriodEnd: now, IssuedAt: &now,
		TaxFacts:      domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK},
		BillingReason: domain.BillingReasonSubscriptionCycle,
	}, nil)
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	draftCN := func(num string) domain.CreditNote {
		t.Helper()
		cn, err := store.Create(ctx, tenantID, domain.CreditNote{
			InvoiceID: inv.ID, CustomerID: cust.ID, CreditNoteNumber: num,
			Status: domain.CreditNoteDraft, Reason: "order_change",
			SubtotalCents: 1000, TotalCents: 1000, CreditAmountCents: 1000,
			Currency: "EUR", RefundStatus: domain.RefundNone,
		})
		if err != nil {
			t.Fatalf("create draft credit note: %v", err)
		}
		return cn
	}

	viaStore := draftCN("CN-SNAP-1")
	if won, err := store.TransitionStatus(ctx, tenantID, viaStore.ID, domain.CreditNoteDraft, domain.CreditNoteIssued); err != nil || !won {
		t.Fatalf("issue via TransitionStatus: won=%v err=%v", won, err)
	}
	viaTx := draftCN("CN-SNAP-2")
	tx, err := store.BeginTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if won, err := store.TransitionStatusTx(ctx, tx, tenantID, viaTx.ID, domain.CreditNoteDraft, domain.CreditNoteIssued); err != nil || !won {
		t.Fatalf("issue via TransitionStatusTx: won=%v err=%v", won, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	stillDraft := draftCN("CN-SNAP-3")

	setParties("After AG", "Neue Strasse 9", "AT", "ATU99999999", "Seller After Inc", "2 New Avenue", "US-EIN-99")

	h := NewHandler(nil, HandlerDeps{Customers: customers, Settings: settings, Invoices: invoices})
	render := func(id string) (BillToInfo, CompanyInfo) {
		t.Helper()
		cn, err := store.Get(ctx, tenantID, id)
		if err != nil {
			t.Fatalf("get credit note: %v", err)
		}
		bt, ci, _ := h.assemblePDFContext(ctx, tenantID, cn)
		return bt, ci
	}
	type want struct{ legal, line1, country, taxID, company, companyLine1, companyTaxID string }
	before := want{"Before GmbH", "Alte Strasse 1", "DE", "DE111111111", "Seller Before Ltd", "1 Old Road", "IE1234567T"}
	after := want{"After AG", "Neue Strasse 9", "AT", "ATU99999999", "Seller After Inc", "2 New Avenue", "US-EIN-99"}
	for _, c := range []struct {
		label string
		id    string
		want  want
	}{
		{"issued via TransitionStatus", viaStore.ID, before},
		{"issued via TransitionStatusTx", viaTx.ID, before},
		{"draft (control)", stillDraft.ID, after},
	} {
		bt, ci := render(c.id)
		got := want{bt.Name, bt.AddressLine1, bt.Country, bt.TaxID, ci.Name, ci.AddressLine1, ci.TaxID}
		if got != c.want {
			t.Errorf("%s: credit-note PDF prints %+v, want %+v", c.label, got, c.want)
		}
	}

	// The stored copy keeps the buyer PII encrypted; the read decrypts it.
	cn, err := store.Get(ctx, tenantID, viaStore.ID)
	if err != nil {
		t.Fatalf("get credit note: %v", err)
	}
	if b := cn.BillToSnapshot; b == nil || b.DisplayName != "CN Buyer" || b.Email != "ap@cn.test" {
		t.Errorf("API bill_to_snapshot = %+v, want plaintext display name and email", b)
	}
	var raw string
	if err := testutil.AdminPool(t).QueryRowContext(ctx,
		`SELECT bill_to_snapshot::text FROM credit_notes WHERE id = $1`, viaStore.ID).Scan(&raw); err != nil {
		t.Fatalf("read raw copy: %v", err)
	}
	for _, pii := range []string{"CN Buyer", "ap@cn.test", before.legal, before.taxID} {
		if strings.Contains(raw, pii) {
			t.Errorf("stored bill_to_snapshot holds plaintext %q: %s", pii, raw)
		}
	}
}
