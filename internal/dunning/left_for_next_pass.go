package dunning

import (
	"github.com/gethardcount/hardcount/internal/domain"
)

// leftForNextPass is work a dunning step left for the next tick:
// domain.ErrLeftForNextRun, with a sentence for the operator that says what
// is left. ProcessDueRuns logs it and goes on.
type leftForNextPass struct {
	say   string
	cause error // may be nil
}

func (e *leftForNextPass) OperatorSafeMessage() string { return e.say }

func (e *leftForNextPass) Error() string {
	if e.cause == nil {
		return e.say
	}
	return e.say + " (" + e.cause.Error() + ")"
}

func (e *leftForNextPass) Unwrap() []error {
	if e.cause == nil {
		return []error{domain.ErrLeftForNextRun}
	}
	return []error{domain.ErrLeftForNextRun, e.cause}
}

// invoiceRef is the run's invoice as the operator knows it: its number when
// the run carries it, else its id.
func invoiceRef(run domain.InvoiceDunningRun) string {
	if run.InvoiceNumber != "" {
		return run.InvoiceNumber
	}
	return run.InvoiceID
}
