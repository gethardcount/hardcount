package invoice

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/crypto"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// ADR-109 (#964): an issued invoice keeps the buyer and supplier details it
// was issued with. These tests run against real Postgres and the real
// customer and settings stores, because the defect lives in what the render
// path reads, not in any one function: the fixture issues an invoice, then
// edits the billing profile and the company settings, then asks every
// invoice render input what it would print.

// partyEra is one complete set of buyer + supplier details. "before" is in
// force when the invoice is issued; "after" is written later.
type partyEra struct {
	legalName, line1, city, postal, country, taxID                   string
	company, companyLine1, companyCity, companyCountry, companyTaxID string
}

var (
	eraBefore = partyEra{
		legalName: "Before GmbH", line1: "Alte Strasse 1", city: "Berlin", postal: "10115", country: "DE", taxID: "DE111111111",
		company: "Seller Before Ltd", companyLine1: "1 Old Road", companyCity: "Dublin", companyCountry: "IE", companyTaxID: "IE1234567T",
	}
	eraAfter = partyEra{
		legalName: "After Pvt Ltd", line1: "99 New Marg", city: "Mumbai", postal: "400001", country: "IN", taxID: "27AAAAA0000A1Z5",
		company: "Seller After Inc", companyLine1: "2 New Avenue", companyCity: "Austin", companyCountry: "US", companyTaxID: "US-EIN-99",
	}
)

type snapRig struct {
	db        *postgres.DB
	ctx       context.Context
	tenantID  string
	custID    string
	store     *PostgresStore
	svc       *Service
	customers *customer.PostgresStore
	settings  *tenant.SettingsStore
}

func newSnapRig(t *testing.T, name string) *snapRig {
	t.Helper()
	return newSnapRigWith(t, name, nil)
}

// newSnapRigWith wires enc into the customer store and the invoice store,
// the way router.go does when VELOX_ENCRYPTION_KEY is set (always, in
// production). nil leaves both stores without a key.
func newSnapRigWith(t *testing.T, name string, enc *crypto.Encryptor) *snapRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, name)
	customers := customer.NewPostgresStore(db)
	settings := tenant.NewSettingsStore(db)
	if enc != nil {
		customers.SetEncryptor(enc)
	}
	cust, err := customers.Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_snap", DisplayName: "Buyer Display", Email: "ap@buyer.test",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	store := NewPostgresStore(db)
	if enc != nil {
		store.SetEncryptor(enc)
	}
	r := &snapRig{
		db: db, ctx: ctx, tenantID: tenantID, custID: cust.ID,
		store: store, svc: NewService(store, clock.Real(), settings),
		customers: customers, settings: settings,
	}
	r.applyEra(t, eraBefore)
	return r
}

// applyEra writes one era into the billing profile and the company settings.
func (r *snapRig) applyEra(t *testing.T, e partyEra) {
	t.Helper()
	if _, err := r.customers.UpsertBillingProfile(r.ctx, r.tenantID, domain.CustomerBillingProfile{
		CustomerID: r.custID, LegalName: e.legalName, AddressLine1: e.line1, City: e.city,
		PostalCode: e.postal, Country: e.country, TaxID: e.taxID, TaxIDType: "vat",
		TaxStatus: domain.TaxStatusStandard, ProfileStatus: domain.BillingProfileReady,
	}); err != nil {
		t.Fatalf("upsert billing profile: %v", err)
	}
	ts, err := r.settings.Get(r.ctx, r.tenantID)
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	ts.CompanyName, ts.CompanyAddressLine1, ts.CompanyCity = e.company, e.companyLine1, e.companyCity
	ts.CompanyCountry, ts.TaxID = e.companyCountry, e.companyTaxID
	ts.CompanyEmail, ts.CompanyPhone = "billing@"+strings.ToLower(strings.Fields(e.company)[1])+".test", "+1 555 "+e.postal
	if _, err := r.settings.Upsert(r.ctx, ts); err != nil {
		t.Fatalf("upsert settings: %v", err)
	}
}

// protoInvoice is a fully-priced invoice in the given status, built the way
// the billing engine builds a cycle invoice.
func (r *snapRig) protoInvoice(num string, status domain.InvoiceStatus, reason domain.InvoiceBillingReason) domain.Invoice {
	now := time.Now().UTC()
	issued := now
	return domain.Invoice{
		CustomerID: r.custID, InvoiceNumber: num, Status: status,
		PaymentStatus: domain.PaymentPending, Currency: "EUR",
		SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
		BillingPeriodStart: now.Add(-30 * 24 * time.Hour), BillingPeriodEnd: now,
		IssuedAt: &issued, NetPaymentTermDays: 30,
		TaxFacts:      domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK},
		BillingReason: reason,
	}
}

func (r *snapRig) createFinalized(t *testing.T, num string) domain.Invoice {
	t.Helper()
	inv, err := r.store.CreateWithLineItems(r.ctx, r.tenantID, r.protoInvoice(num, domain.InvoiceFinalized, domain.BillingReasonSubscriptionCycle), nil)
	if err != nil {
		t.Fatalf("create finalized %s: %v", num, err)
	}
	return inv
}

// pdfParties re-reads the invoice and returns what every invoice PDF
// (emailed, API download, hosted PDF) would print for it.
func (r *snapRig) pdfParties(t *testing.T, id string) (domain.Invoice, BillToInfo, CompanyInfo) {
	t.Helper()
	inv, err := r.store.Get(r.ctx, r.tenantID, id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	bt, ci, _ := BuildPDFContext(r.ctx, r.customers, r.settings, nil, r.tenantID, &inv)
	return inv, bt, ci
}

// assertPrints checks every printed party field against one era.
func assertPrints(t *testing.T, label string, inv domain.Invoice, bt BillToInfo, ci CompanyInfo, e partyEra) {
	t.Helper()
	checks := []struct{ field, got, want string }{
		{"bill-to name", bt.Name, e.legalName},
		{"bill-to line1", bt.AddressLine1, e.line1},
		{"bill-to city", bt.City, e.city},
		{"bill-to postal", bt.PostalCode, e.postal},
		{"bill-to country", bt.Country, e.country},
		{"printed buyer tax ID", buyerTaxID(inv, bt), e.taxID},
		{"supplier name", ci.Name, e.company},
		{"supplier line1", ci.AddressLine1, e.companyLine1},
		{"supplier city", ci.City, e.companyCity},
		{"supplier country", ci.Country, e.companyCountry},
		{"supplier tax ID", ci.TaxID, e.companyTaxID},
		{"supplier tax ID type", ci.TaxIDType, SupplierTaxIDTypeFromCountry(e.companyCountry)},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: %s = %q, want %q", label, c.field, c.got, c.want)
		}
	}
}

// TestIssuedInvoice_PDFKeepsIssueTimeParties is the #964 reproduction: issue,
// edit the profile and company, render. The PDF must print the issue-time
// details. The draft created alongside it is the control: it must follow the
// edit, which proves the edit landed and that drafts still render live.
func TestIssuedInvoice_PDFKeepsIssueTimeParties(t *testing.T) {
	r := newSnapRig(t, "Snapshot PDF Corp")
	issued := r.createFinalized(t, "SNAP-PDF-1")
	draft, err := r.store.CreateWithLineItems(r.ctx, r.tenantID, r.protoInvoice("", domain.InvoiceDraft, domain.BillingReasonSubscriptionCycle), nil)
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}

	r.applyEra(t, eraAfter)

	inv, bt, ci := r.pdfParties(t, issued.ID)
	assertPrints(t, "issued invoice after the edit", inv, bt, ci, eraBefore)

	dinv, dbt, dci := r.pdfParties(t, draft.ID)
	assertPrints(t, "draft after the edit (control)", dinv, dbt, dci, eraAfter)
}

// TestIssuedInvoice_FinalizedAfterEditTakesNewParties: the copy is taken at
// finalize, not at draft creation. A draft built under the old details and
// finalized after the edit is issued with the new ones; a further edit after
// that does not move it.
func TestIssuedInvoice_FinalizedAfterEditTakesNewParties(t *testing.T) {
	r := newSnapRig(t, "Snapshot Late Finalize Corp")
	draft, err := r.store.CreateWithLineItems(r.ctx, r.tenantID, r.protoInvoice("SNAP-LATE-1", domain.InvoiceDraft, domain.BillingReasonSubscriptionCycle), nil)
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	r.applyEra(t, eraAfter)
	if _, err := r.svc.Finalize(r.ctx, r.tenantID, draft.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	r.applyEra(t, eraBefore)

	inv, bt, ci := r.pdfParties(t, draft.ID)
	assertPrints(t, "invoice finalized after the first edit", inv, bt, ci, eraAfter)
}

// TestIssuedInvoice_NullSnapshotFallsBackToLive: an invoice issued before
// migration 0177 has no copy. It must keep rendering from live data exactly
// as it always has (the ADR forbids inventing a copy for it).
func TestIssuedInvoice_NullSnapshotFallsBackToLive(t *testing.T) {
	r := newSnapRig(t, "Snapshot Legacy Corp")
	issued := r.createFinalized(t, "SNAP-LEGACY-1")
	admin := testutil.AdminPool(t)
	if _, err := admin.ExecContext(r.ctx,
		`UPDATE invoices SET bill_to_snapshot = NULL, supplier_snapshot = NULL WHERE id = $1`, issued.ID); err != nil {
		t.Fatalf("clear snapshot to model a pre-0177 row: %v", err)
	}
	r.applyEra(t, eraAfter)

	inv, bt, ci := r.pdfParties(t, issued.ID)
	if inv.BillToSnapshot != nil || inv.SupplierSnapshot != nil {
		t.Fatalf("legacy row read back with a snapshot (%+v / %+v): a NULL column must scan as nil, never as an empty copy", inv.BillToSnapshot, inv.SupplierSnapshot)
	}
	assertPrints(t, "pre-0177 invoice", inv, bt, ci, eraAfter)
}

// TestIssuedInvoice_ReverseChargeLegendUsesSnapshot: the legend wording
// branches on buyer and supplier country. Issued under EU details, the
// wording must stay the EU text after the buyer moves to India.
func TestIssuedInvoice_ReverseChargeLegendUsesSnapshot(t *testing.T) {
	r := newSnapRig(t, "Snapshot Legend Corp")
	proto := r.protoInvoice("SNAP-RC-1", domain.InvoiceFinalized, domain.BillingReasonSubscriptionCycle)
	proto.TaxReverseCharge = true
	issued, err := r.store.CreateWithLineItems(r.ctx, r.tenantID, proto, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	r.applyEra(t, eraAfter) // buyer country IN: the live wording would flip to GST

	inv, bt, ci := r.pdfParties(t, issued.ID)
	got := reverseChargeLegend(inv, ci, bt, nil)
	if !strings.HasPrefix(got, "Reverse charge — VAT") {
		t.Errorf("legend after the buyer moved to IN = %q, want the EU wording it was issued with", got)
	}
}

// TestIssuedInvoice_EveryFinalizeWriterStamps proves each SQL statement that
// takes an invoice out of draft stamps the copy, reached through the Go entry
// point that production uses for it. Each case issues under eraBefore, then
// edits, then renders: a writer that skipped the stamp prints eraAfter.
//
// Go path → SQL site:
//   - cycle close, threshold, BillOnCreate (non-tx): CreateWithLineItems(Audited) → createWithLineItemsInTx
//   - final-on-cancel, day-1 in the create tx, proration/swap: CreateWithLineItemsTx → createWithLineItemsInTx
//   - operator Create: CreateAudited
//   - engine draft finalize (paused/tax-deferred cycle draft): Service.Finalize → updateStatusInTx
//   - operator-composed finalize: Service.Finalize → FinalizeWithDates
//   - tax-retry auto-finalize and the profile-save flush: RetryTax / RetryCustomerDataErrors → updateStatusInTx
//
// The billing engine's own entry points are covered end-to-end in
// internal/billing/issued_snapshot_integration_test.go.
func TestIssuedInvoice_EveryFinalizeWriterStamps(t *testing.T) {
	cases := []struct {
		name  string
		issue func(t *testing.T, r *snapRig) string
	}{
		{"born finalized: CreateWithLineItems", func(t *testing.T, r *snapRig) string {
			return r.createFinalized(t, "SNAP-W-1").ID
		}},
		{"born finalized: CreateWithLineItemsAudited", func(t *testing.T, r *snapRig) string {
			inv, err := r.store.CreateWithLineItemsAudited(r.ctx, r.tenantID, r.protoInvoice("SNAP-W-2", domain.InvoiceFinalized, domain.BillingReasonThreshold), nil, nil)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			return inv.ID
		}},
		{"born finalized: CreateWithLineItemsTx (coordinator tx)", func(t *testing.T, r *snapRig) string {
			var id string
			if err := r.db.WithTenantTx(r.ctx, r.tenantID, func(tx *sql.Tx) error {
				inv, err := r.store.CreateWithLineItemsTx(r.ctx, tx, r.tenantID, r.protoInvoice("SNAP-W-3", domain.InvoiceFinalized, domain.BillingReasonSubscriptionUpdate), nil)
				id = inv.ID
				return err
			}); err != nil {
				t.Fatalf("create in tx: %v", err)
			}
			return id
		}},
		{"born finalized: Create (CreateAudited)", func(t *testing.T, r *snapRig) string {
			inv, err := r.store.Create(r.ctx, r.tenantID, r.protoInvoice("SNAP-W-4", domain.InvoiceFinalized, domain.BillingReasonManual))
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			return inv.ID
		}},
		{"engine draft: Service.Finalize → UpdateStatus", func(t *testing.T, r *snapRig) string {
			d, err := r.store.CreateWithLineItems(r.ctx, r.tenantID, r.protoInvoice("SNAP-W-5", domain.InvoiceDraft, domain.BillingReasonSubscriptionCycle), nil)
			if err != nil {
				t.Fatalf("create draft: %v", err)
			}
			if _, err := r.svc.Finalize(r.ctx, r.tenantID, d.ID); err != nil {
				t.Fatalf("finalize: %v", err)
			}
			return d.ID
		}},
		{"operator draft: Service.Finalize → FinalizeWithDates", func(t *testing.T, r *snapRig) string {
			d, err := r.svc.Create(r.ctx, r.tenantID, CreateInput{CustomerID: r.custID})
			if err != nil {
				t.Fatalf("create manual draft: %v", err)
			}
			if _, err := r.svc.Finalize(r.ctx, r.tenantID, d.ID); err != nil {
				t.Fatalf("finalize: %v", err)
			}
			return d.ID
		}},
		{"tax retry auto-finalize: RetryTax → UpdateStatus", func(t *testing.T, r *snapRig) string {
			id := r.taxPendingDraft(t, "SNAP-W-7")
			if _, err := r.svc.RetryTax(r.ctx, r.tenantID, id); err != nil {
				t.Fatalf("retry tax: %v", err)
			}
			return id
		}},
		{"profile-save flush: RetryCustomerDataErrors → UpdateStatus", func(t *testing.T, r *snapRig) string {
			id := r.taxPendingDraft(t, "SNAP-W-8")
			if n, errs := r.svc.RetryCustomerDataErrors(r.ctx, r.tenantID, r.custID); n != 1 || len(errs) > 0 {
				t.Fatalf("RetryCustomerDataErrors = %d, %v; want 1 and no errors", n, errs)
			}
			return id
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newSnapRig(t, "Snapshot Writer Corp")
			id := c.issue(t, r)
			inv, err := r.store.Get(r.ctx, r.tenantID, id)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if inv.Status != domain.InvoiceFinalized {
				t.Fatalf("status = %s, want finalized (the path under test did not issue)", inv.Status)
			}
			r.applyEra(t, eraAfter)
			inv, bt, ci := r.pdfParties(t, id)
			if inv.BillToSnapshot == nil || inv.SupplierSnapshot == nil {
				t.Errorf("issued through %q with no stored copy (bill-to %v, supplier %v)", c.name, inv.BillToSnapshot != nil, inv.SupplierSnapshot != nil)
			}
			assertPrints(t, c.name, inv, bt, ci, eraBefore)
		})
	}
}

// TestIssuedInvoice_ReturnedRowCarriesSnapshot: callers that render from the
// value the writer hands back (rather than re-reading) must see the copy too.
// ADR-109's "RETURNING runs before the stamp" trap.
func TestIssuedInvoice_ReturnedRowCarriesSnapshot(t *testing.T) {
	r := newSnapRig(t, "Snapshot Returning Corp")
	born := r.createFinalized(t, "SNAP-RET-1")
	if born.BillToSnapshot == nil || born.BillToSnapshot.LegalName != eraBefore.legalName {
		t.Errorf("CreateWithLineItems returned bill-to copy %+v, want legal name %q", born.BillToSnapshot, eraBefore.legalName)
	}
	d, err := r.store.CreateWithLineItems(r.ctx, r.tenantID, r.protoInvoice("SNAP-RET-2", domain.InvoiceDraft, domain.BillingReasonSubscriptionCycle), nil)
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if d.BillToSnapshot != nil {
		t.Errorf("draft carries a copy %+v; drafts must render live", d.BillToSnapshot)
	}
	fin, err := r.svc.Finalize(r.ctx, r.tenantID, d.ID)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if fin.SupplierSnapshot == nil || fin.SupplierSnapshot.Name != eraBefore.company {
		t.Errorf("Finalize returned supplier copy %+v, want company %q", fin.SupplierSnapshot, eraBefore.company)
	}
}

// taxPendingDraft is a cycle draft parked on a customer-data tax error, the
// shape RetryCustomerDataErrors flushes when the operator saves the profile.
func (r *snapRig) taxPendingDraft(t *testing.T, num string) string {
	t.Helper()
	p := r.protoInvoice(num, domain.InvoiceDraft, domain.BillingReasonSubscriptionCycle)
	p.TaxStatus = domain.InvoiceTaxPending
	p.TaxErrorCode = "customer_data_invalid"
	d, err := r.store.CreateWithLineItems(r.ctx, r.tenantID, p, nil)
	if err != nil {
		t.Fatalf("create tax-pending draft: %v", err)
	}
	r.svc.SetTaxRetrier(&resolvingTaxRetrier{db: r.db, store: r.store})
	return d.ID
}

// resolvingTaxRetrier stands in for the billing engine's tax recompute: it
// marks the invoice's tax resolved on the real row and returns it, which is
// all RetryTax needs before it auto-finalizes.
type resolvingTaxRetrier struct {
	db    *postgres.DB
	store *PostgresStore
}

func (f *resolvingTaxRetrier) RetryTaxForInvoice(ctx context.Context, tenantID, invoiceID string) (domain.Invoice, error) {
	if err := f.db.WithTenantTx(ctx, tenantID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE invoices SET tax_status = 'ok', tax_error_code = NULL WHERE id = $1`, invoiceID)
		return err
	}); err != nil {
		return domain.Invoice{}, err
	}
	return f.store.Get(ctx, tenantID, invoiceID)
}

func (f *resolvingTaxRetrier) ComputeTaxForInvoice(ctx context.Context, tenantID, invoiceID string) (domain.Invoice, error) {
	return f.store.Get(ctx, tenantID, invoiceID)
}

// TestIssuedInvoice_BuyerTaxIDIsTheInvoicesOwn: when the invoice carries the
// tax ID its tax was calculated with, the copy records that ID, so the PDF
// prints one buyer tax ID from one place even after the profile changes.
func TestIssuedInvoice_BuyerTaxIDIsTheInvoicesOwn(t *testing.T) {
	r := newSnapRig(t, "Snapshot Tax ID Corp")
	p := r.protoInvoice("SNAP-TAXID-1", domain.InvoiceFinalized, domain.BillingReasonSubscriptionCycle)
	p.TaxID = "DE999000111"
	issued, err := r.store.CreateWithLineItems(r.ctx, r.tenantID, p, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if issued.BillToSnapshot == nil || issued.BillToSnapshot.TaxID != "DE999000111" {
		t.Fatalf("bill-to copy %+v, want tax_id DE999000111 (the invoice's own)", issued.BillToSnapshot)
	}
	r.applyEra(t, eraAfter)
	inv, bt, _ := r.pdfParties(t, issued.ID)
	if got := buyerTaxID(inv, bt); got != "DE999000111" {
		t.Errorf("printed buyer tax ID = %q, want DE999000111", got)
	}
}

// TestIssuedInvoice_EncryptedPIIPrintsPlaintextAndStaysEncrypted: production
// encrypts the customer's display name and email and the billing profile's
// legal name and tax ID at rest. The issue stamp must decrypt them before it
// copies them, so every render prints plaintext, and must encrypt them again
// in the copy, so the invoice row holds no more plaintext PII than the
// customer rows do.
func TestIssuedInvoice_EncryptedPIIPrintsPlaintextAndStaysEncrypted(t *testing.T) {
	enc, err := crypto.NewEncryptor(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("encryptor: %v", err)
	}
	r := newSnapRigWith(t, "Snapshot Encrypted Corp", enc)
	issued := r.createFinalized(t, "SNAP-ENC-1")
	r.applyEra(t, eraAfter)

	inv, bt, ci := r.pdfParties(t, issued.ID)
	assertPrints(t, "issued invoice, encryption on", inv, bt, ci, eraBefore)
	if b := inv.BillToSnapshot; b == nil || b.DisplayName != "Buyer Display" || b.Email != "ap@buyer.test" {
		t.Errorf("API bill_to_snapshot = %+v, want plaintext display name and email", b)
	}

	var raw string
	if err := testutil.AdminPool(t).QueryRowContext(r.ctx,
		`SELECT bill_to_snapshot::text FROM invoices WHERE id = $1`, issued.ID).Scan(&raw); err != nil {
		t.Fatalf("read raw copy: %v", err)
	}
	for _, pii := range []string{"Buyer Display", "ap@buyer.test", eraBefore.legalName, eraBefore.taxID} {
		if strings.Contains(raw, pii) {
			t.Errorf("stored bill_to_snapshot holds plaintext %q: %s", pii, raw)
		}
	}
	if !strings.Contains(raw, "enc:") {
		t.Errorf("stored bill_to_snapshot has no encrypted field: %s", raw)
	}
}

// TestIssuedInvoice_LaterTransitionsKeepIssueCopy: only taking an invoice
// out of draft stamps the copy. Voiding it, writing it off or recording its
// payment after a profile edit must leave the issue-time details in place.
func TestIssuedInvoice_LaterTransitionsKeepIssueCopy(t *testing.T) {
	cases := []struct {
		name string
		move func(t *testing.T, r *snapRig, id string) (domain.Invoice, error)
	}{
		{"void", func(t *testing.T, r *snapRig, id string) (domain.Invoice, error) {
			return r.store.UpdateStatus(r.ctx, r.tenantID, id, domain.InvoiceVoided)
		}},
		{"mark uncollectible", func(t *testing.T, r *snapRig, id string) (domain.Invoice, error) {
			return r.store.UpdateStatus(r.ctx, r.tenantID, id, domain.InvoiceUncollectible)
		}},
		{"mark paid", func(t *testing.T, r *snapRig, id string) (domain.Invoice, error) {
			return r.store.MarkPaid(r.ctx, r.tenantID, id, "", time.Now().UTC())
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newSnapRig(t, "Snapshot Transition Corp")
			issued := r.createFinalized(t, "SNAP-MOVE-1")
			r.applyEra(t, eraAfter)
			moved, err := c.move(t, r, issued.ID)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if moved.Status == domain.InvoiceFinalized {
				t.Fatalf("%s left the invoice finalized; the transition under test did not run", c.name)
			}
			inv, bt, ci := r.pdfParties(t, issued.ID)
			assertPrints(t, "after "+c.name, inv, bt, ci, eraBefore)
		})
	}
}
