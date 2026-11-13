package api

import (
	"context"
	"errors"
	"testing"

	"github.com/gethardcount/hardcount/internal/billing"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/tax"
)

// failingSettings is a settings reader whose read fails.
type failingSettings struct{ billing.SettingsReader }

func (failingSettings) Get(context.Context, string) (domain.TenantSettings, error) {
	return domain.TenantSettings{}, errors.New("read tenant settings: connection refused")
}

// When the engine cannot read the tax settings, the proration adapter returns
// the error, not a result with the tax left pending: the proration writer
// refuses the change on it (503 proration_unavailable) instead of writing a
// draft it cannot tax.
func TestProrationTaxAdapter_SettingsReadFails_ReturnsTheError(t *testing.T) {
	e := billing.NewEngine(nil, nil, nil, nil, nil, failingSettings{}, nil, nil, clock.Real())
	e.SetTaxProviderResolver(tax.NewResolver(nil))
	a := &prorationTaxApplierAdapter{engine: e}

	r, err := a.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", 1000, 0,
		[]domain.InvoiceLineItem{{LineType: domain.LineTypeBaseFee, Quantity: 1, UnitAmountCents: 1000, AmountCents: 1000, TotalAmountCents: 1000, Currency: "USD"}})
	if err == nil {
		t.Fatalf("err = nil, result %+v; want the settings read's error", r)
	}
	if r.TaxStatus == domain.InvoiceTaxPending {
		t.Errorf("result tax status = pending; want no result beside the error")
	}
}
