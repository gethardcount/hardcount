package domain

// PricedTaxError is a tax failure on invoice lines whose prices are fixed:
// the settings that priced them were read, so it is known whether the prices
// include tax. A writer that keeps the invoice with its tax left pending
// records TaxInclusive on it. An error that does not carry one means the
// settings were not read and the prices' meaning is not known.
type PricedTaxError struct {
	TaxInclusive bool
	Err          error
}

// Error is the cause's message, so classifying the failure (tax.Classify
// reads the text) sees the same words.
func (e *PricedTaxError) Error() string { return e.Err.Error() }

func (e *PricedTaxError) Unwrap() error { return e.Err }
