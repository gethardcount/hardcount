package domain

import (
	"encoding/json"
	"fmt"
)

// DocumentBillTo is what an issued document prints about its buyer, copied
// when the document is issued (ADR-109). An invoice or credit note issued
// after migration 0177 renders its bill-to block from this copy, so a later
// edit to the customer or billing profile does not change it.
//
// It holds the raw inputs (display name and legal name) rather than one
// resolved name, so a renderer can change how it prints them without losing
// what was true at issue.
type DocumentBillTo struct {
	DisplayName  string `json:"display_name"`
	LegalName    string `json:"legal_name,omitempty"`
	Email        string `json:"email,omitempty"`
	AddressLine1 string `json:"address_line1,omitempty"`
	AddressLine2 string `json:"address_line2,omitempty"`
	City         string `json:"city,omitempty"`
	State        string `json:"state,omitempty"`
	PostalCode   string `json:"postal_code,omitempty"`
	Country      string `json:"country,omitempty"`
	TaxID        string `json:"tax_id,omitempty"`
	TaxIDType    string `json:"tax_id_type,omitempty"`
}

// Name is the buyer name a document prints: the legal name when the billing
// profile has one, otherwise the customer's display name.
func (b DocumentBillTo) Name() string {
	if b.LegalName != "" {
		return b.LegalName
	}
	return b.DisplayName
}

// DocumentSupplier is what an issued document prints about its seller (the
// tenant's company), copied when the document is issued (ADR-109). Branding
// (logo, brand color, support URL) is not part of it and stays live.
type DocumentSupplier struct {
	Name         string `json:"name,omitempty"`
	Email        string `json:"email,omitempty"`
	Phone        string `json:"phone,omitempty"`
	AddressLine1 string `json:"address_line1,omitempty"`
	AddressLine2 string `json:"address_line2,omitempty"`
	City         string `json:"city,omitempty"`
	State        string `json:"state,omitempty"`
	PostalCode   string `json:"postal_code,omitempty"`
	Country      string `json:"country,omitempty"`
	TaxID        string `json:"tax_id,omitempty"`
}

// NewDocumentBillTo builds the bill-to block from live rows. bp is nil when
// the customer has no billing profile. The live render path and the issue
// stamp both use it, so a draft and the document it becomes print the same
// fields from the same rows.
func NewDocumentBillTo(c Customer, bp *CustomerBillingProfile) DocumentBillTo {
	b := DocumentBillTo{DisplayName: c.DisplayName, Email: c.Email}
	if bp != nil {
		b.LegalName = bp.LegalName
		b.AddressLine1 = bp.AddressLine1
		b.AddressLine2 = bp.AddressLine2
		b.City = bp.City
		b.State = bp.State
		b.PostalCode = bp.PostalCode
		b.Country = bp.Country
		b.TaxID = bp.TaxID
		b.TaxIDType = bp.TaxIDType
	}
	return b
}

// NewDocumentSupplier builds the supplier block from live tenant settings.
func NewDocumentSupplier(ts TenantSettings) DocumentSupplier {
	return DocumentSupplier{
		Name:         ts.CompanyName,
		Email:        ts.CompanyEmail,
		Phone:        ts.CompanyPhone,
		AddressLine1: ts.CompanyAddressLine1,
		AddressLine2: ts.CompanyAddressLine2,
		City:         ts.CompanyCity,
		State:        ts.CompanyState,
		PostalCode:   ts.CompanyPostalCode,
		Country:      ts.CompanyCountry,
		TaxID:        ts.TaxID,
	}
}

// DocumentPartiesSQL reads, inside the issuing transaction, every live field
// the two blocks copy. $1 is the customer id. It runs on the caller's
// RLS-scoped transaction, so it sees the same tenant and mode as the document
// row being issued. A missing billing profile or settings row reads as empty
// strings; a missing customer returns no row, and the stamp fails instead of
// writing an empty copy. Scan with DocumentPartiesScanDest. The bill-to PII
// columns come back as stored (encrypted when a key is set): call
// DocumentBillTo.Open before using them.
const DocumentPartiesSQL = `
	SELECT c.display_name, COALESCE(c.email,''),
		COALESCE(bp.legal_name,''),
		COALESCE(bp.address_line1,''), COALESCE(bp.address_line2,''),
		COALESCE(bp.city,''), COALESCE(bp.state,''), COALESCE(bp.postal_code,''),
		COALESCE(bp.country,''), COALESCE(bp.tax_id,''), COALESCE(bp.tax_id_type,''),
		COALESCE(ts.company_name,''), COALESCE(ts.company_email,''), COALESCE(ts.company_phone,''),
		COALESCE(ts.company_address_line1,''), COALESCE(ts.company_address_line2,''),
		COALESCE(ts.company_city,''), COALESCE(ts.company_state,''), COALESCE(ts.company_postal_code,''),
		COALESCE(ts.company_country,''), COALESCE(ts.tax_id,'')
	FROM customers c
	LEFT JOIN customer_billing_profiles bp ON bp.customer_id = c.id AND bp.tenant_id = c.tenant_id
	LEFT JOIN tenant_settings ts ON ts.tenant_id = c.tenant_id
	WHERE c.id = $1`

// DocumentPartiesScanDest returns Scan destinations in DocumentPartiesSQL
// column order.
func DocumentPartiesScanDest(b *DocumentBillTo, s *DocumentSupplier) []any {
	return []any{
		&b.DisplayName, &b.Email,
		&b.LegalName,
		&b.AddressLine1, &b.AddressLine2,
		&b.City, &b.State, &b.PostalCode,
		&b.Country, &b.TaxID, &b.TaxIDType,
		&s.Name, &s.Email, &s.Phone,
		&s.AddressLine1, &s.AddressLine2,
		&s.City, &s.State, &s.PostalCode,
		&s.Country, &s.TaxID,
	}
}

// SnapshotScanner scans a nullable JSONB snapshot column into *T. SQL NULL
// leaves the destination nil, which is what tells a renderer to fall back to
// live data. The column must be selected bare: a COALESCE would turn NULL
// into a value and the fallback could never fire (ADR-109 trap).
//
// Open, when set, runs on the decoded value before it is stored, so a store
// can decrypt the bill-to PII fields on the way out (DocumentBillTo.Open).
type SnapshotScanner[T any] struct {
	Dst  **T
	Open func(T) (T, error)
}

// Scan implements sql.Scanner.
func (s SnapshotScanner[T]) Scan(src any) error {
	*s.Dst = nil
	var raw []byte
	switch v := src.(type) {
	case nil:
		return nil
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		return fmt.Errorf("document snapshot: unsupported column type %T", src)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("document snapshot: %w", err)
	}
	if s.Open != nil {
		opened, err := s.Open(out)
		if err != nil {
			return fmt.Errorf("document snapshot: %w", err)
		}
		out = opened
	}
	*s.Dst = &out
	return nil
}

// FieldCipher encrypts and decrypts one stored string. *crypto.Encryptor
// satisfies it; with no key it passes plaintext through.
type FieldCipher interface {
	Encrypt(plaintext string) (string, error)
	Decrypt(stored string) (string, error)
}

// Seal encrypts the bill-to fields the customer store encrypts at rest
// (display name, email, legal name, tax ID), so the issued copy holds no
// more plaintext PII than the rows it was copied from. Address fields stay
// plaintext, as they are on the billing profile.
func (b DocumentBillTo) Seal(c FieldCipher) (DocumentBillTo, error) {
	return b.mapPII(c.Encrypt)
}

// Open is the inverse of Seal. It also decrypts the raw customer and
// billing-profile columns DocumentPartiesSQL reads, which hold the same
// fields encrypted. A plaintext value passes through unchanged.
func (b DocumentBillTo) Open(c FieldCipher) (DocumentBillTo, error) {
	return b.mapPII(c.Decrypt)
}

func (b DocumentBillTo) mapPII(f func(string) (string, error)) (DocumentBillTo, error) {
	for _, field := range []struct {
		name string
		v    *string
	}{
		{"display_name", &b.DisplayName},
		{"email", &b.Email},
		{"legal_name", &b.LegalName},
		{"tax_id", &b.TaxID},
	} {
		out, err := f(*field.v)
		if err != nil {
			return DocumentBillTo{}, fmt.Errorf("bill-to %s: %w", field.name, err)
		}
		*field.v = out
	}
	return b, nil
}
