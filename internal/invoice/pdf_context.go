package invoice

import (
	"context"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// BuildPDFContext assembles everything RenderPDF needs beyond the
// invoice and its lines — bill-to (customer + billing profile), company
// header (tenant settings), and the invoice's issued credit notes — in
// ONE place. The emailed, dashboard-download, and hosted-page PDFs each
// hand-rolled this block and had drifted three ways: the emailed PDF
// carried no buyer address and no credit notes (an emailed invoice
// showed a different amount-due story than the downloaded one), and no
// invoice surface carried the buyer's tax ID at all (the credit-note
// PDF did — EU VAT/GST compliance needs it on invoices too).
//
// An issued invoice renders its bill-to and supplier blocks from the copy
// taken when it was issued (inv.BillToSnapshot / inv.SupplierSnapshot,
// ADR-109), so a later profile or company edit does not change it. A draft,
// or an invoice issued before migration 0177 (nil copy), renders from live
// rows. Brand color and the period timezone are read live either way: they
// are not part of the issued document's content.
//
// Every dependency is optional (nil-tolerant) and every lookup is
// best-effort: a missing profile or settings row degrades that block,
// never the render. inv is a pointer so the builder can stamp
// BillingPeriodDisplay when the caller's fetch path bypassed the
// service read decorator that normally sets it (the hosted
// GetByPublicToken path) — it never overwrites a non-empty value.
func BuildPDFContext(
	ctx context.Context,
	customers CustomerGetter,
	settings SettingsGetter,
	creditNotes CreditNoteLister,
	tenantID string,
	inv *domain.Invoice,
) (BillToInfo, CompanyInfo, []CreditNoteInfo) {
	var bt BillToInfo
	if inv.BillToSnapshot != nil {
		bt = billToInfo(*inv.BillToSnapshot)
	} else {
		// The customer id stands in for the name when the customer read
		// fails; the profile still applies.
		cust := domain.Customer{DisplayName: inv.CustomerID}
		var profile *domain.CustomerBillingProfile
		if customers != nil {
			if c, err := customers.Get(ctx, tenantID, inv.CustomerID); err == nil {
				cust = c
			}
			if bp, err := customers.GetBillingProfile(ctx, tenantID, inv.CustomerID); err == nil {
				profile = &bp
			}
		}
		bt = billToInfo(domain.NewDocumentBillTo(cust, profile))
	}

	var ci CompanyInfo
	if inv.SupplierSnapshot != nil {
		ci = companyInfo(*inv.SupplierSnapshot)
	}
	if settings != nil {
		if ts, err := settings.Get(ctx, tenantID); err == nil {
			if inv.SupplierSnapshot == nil {
				ci = companyInfo(domain.NewDocumentSupplier(ts))
			}
			ci.BrandColor = ts.BrandColor
			// Inclusive-last-day period string (ADR-058): fetch paths that
			// bypass the service read decorator (hosted GetByPublicToken)
			// arrive without it — author it here from the same domain helper.
			// Anchored in the invoice's own denormalized billing TZ (ADR-077),
			// falling back to the tenant TZ for ad-hoc/legacy invoices — the
			// same resolution the service decorator (invoiceDisplayLoc) uses.
			if inv.BillingPeriodDisplay == "" {
				periodLoc := domain.LoadLocationOrUTC(ts.Timezone)
				if inv.BillingTimezone != "" {
					periodLoc = domain.LoadLocationOrUTC(inv.BillingTimezone)
				}
				inv.BillingPeriodDisplay = domain.FormatInclusivePeriod(inv.BillingPeriodStart, inv.BillingPeriodEnd, periodLoc)
			}
		}
	}

	var cnInfos []CreditNoteInfo
	if creditNotes != nil {
		if notes, err := creditNotes.List(ctx, tenantID, inv.ID); err == nil {
			for _, cn := range notes {
				if cn.Status != domain.CreditNoteIssued {
					continue
				}
				cnInfos = append(cnInfos, CreditNoteInfo{
					Number:               cn.CreditNoteNumber,
					Reason:               cn.Reason,
					Amount:               cn.TotalCents,
					RefundAmountCents:    cn.RefundAmountCents,
					CreditAmountCents:    cn.CreditAmountCents,
					OutOfBandAmountCents: cn.OutOfBandAmountCents,
					TaxAmountCents:       cn.TaxAmountCents,
					TaxTransactionID:     cn.TaxTransactionID,
					RefundStatus:         string(cn.RefundStatus),
				})
			}
		}
	}

	return bt, ci, cnInfos
}

// billToInfo maps the bill-to block, issued copy or live, onto what the PDF
// prints.
func billToInfo(b domain.DocumentBillTo) BillToInfo {
	return BillToInfo{
		Name:         b.Name(),
		Email:        b.Email,
		AddressLine1: b.AddressLine1,
		AddressLine2: b.AddressLine2,
		City:         b.City,
		State:        b.State,
		PostalCode:   b.PostalCode,
		Country:      b.Country,
		TaxID:        b.TaxID,
	}
}

// companyInfo maps the supplier block onto the PDF header. The tax-ID type is
// derived from the supplier country, which is itself part of the block, so
// the label and the reverse-charge wording are fixed on an issued invoice.
func companyInfo(s domain.DocumentSupplier) CompanyInfo {
	return CompanyInfo{
		Name:         s.Name,
		Email:        s.Email,
		Phone:        s.Phone,
		AddressLine1: s.AddressLine1,
		AddressLine2: s.AddressLine2,
		City:         s.City,
		State:        s.State,
		PostalCode:   s.PostalCode,
		Country:      s.Country,
		TaxID:        s.TaxID,
		TaxIDType:    SupplierTaxIDTypeFromCountry(s.Country),
	}
}
