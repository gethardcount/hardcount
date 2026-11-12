package api

import (
	"reflect"
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// The Checkout expiry after an offline payment and after a credit note is
// wired by setters, and an unwired one is skipped. A forgotten wiring line
// would leave a customer's open pay page payable after the invoice stopped
// asking for that money, with every other test green. This pins that
// NewServer sets both. It goes when the two services take their dependencies
// at construction.
func TestNewServer_WiresCheckoutExpiry(t *testing.T) {
	srv := NewServer(&postgres.DB{}, nil, nil)
	for _, c := range []struct {
		name    string
		service any
		field   string
	}{
		{"invoice service (offline payment, write-off, void)", srv.InvoiceSvc, "collectionStopper"},
		{"credit-note service (a note that lowers what is owed)", srv.CreditNoteSvc, "checkout"},
	} {
		f := reflect.ValueOf(c.service).Elem().FieldByName(c.field)
		if !f.IsValid() {
			t.Fatalf("%s has no field %q; if it moved, move this pin", c.name, c.field)
		}
		if f.IsNil() {
			t.Errorf("%s: %s is not wired in NewServer, so its Checkout expiry is skipped", c.name, c.field)
		}
	}
}
