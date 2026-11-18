package errs

import (
	"errors"
	"fmt"
)

// DomainError is an error the API can answer: Kind picks the status, Code
// replaces the status's default code where the status takes one, Field names
// the input at fault and Message is the error's text. Cause is for the log;
// only Unavailable sets it. With no Kind it is a server fault and shows
// nothing. respond.FromError has the mapping, row by row.
type DomainError struct {
	Kind    error
	Code    string
	Field   string
	Message string
	Cause   error
}

// Error returns Message, followed by Cause when there is one.
func (e *DomainError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

// Unwrap returns Cause, so errors.Is and errors.As look through it.
func (e *DomainError) Unwrap() error { return e.Cause }

// WithCode sets e's Code and returns e, to chain onto a constructor:
//
//	return errs.AlreadyExists("invoice_number", "invoice number taken").WithCode("invoice_number_taken")
//
// A code is part of the API, because clients switch on it: change one only
// when the failure it names changes.
func (e *DomainError) WithCode(code string) *DomainError {
	e.Code = code
	return e
}

// Is reports whether target is e's Kind, so errors.Is(err, ErrValidation)
// finds a DomainError of that kind anywhere in err's chain.
func (e *DomainError) Is(target error) bool {
	if e.Kind == nil {
		return false
	}
	return e.Kind == target
}

// Code returns the Code of a DomainError anywhere in the error chain, or "".
func Code(err error) string {
	var de *DomainError
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

// Field returns the Field of a DomainError anywhere in the error chain, or "".
func Field(err error) string {
	var de *DomainError
	if errors.As(err, &de) {
		return de.Field
	}
	return ""
}

// Required is the validation error for a missing field: "<field> is
// required", with field as its param.
//
//	return errs.Required("external_id")
func Required(field string) *DomainError {
	return &DomainError{
		Kind:    ErrValidation,
		Field:   field,
		Message: field + " is required",
	}
}

// Invalid is the validation error for a field that was sent but failed a
// check. The client shows reason under the field, so phrase it to read there,
// such as "must be a valid email address".
//
//	return errs.Invalid("company_email", "must contain @ and a domain")
func Invalid(field, reason string) *DomainError {
	return &DomainError{
		Kind:    ErrValidation,
		Field:   field,
		Message: reason,
	}
}

// AlreadyExists is the error for a value that must be unique and is taken,
// such as a customer's external_id. field, when set, is its param.
//
//	return errs.AlreadyExists("code", fmt.Sprintf("plan code %q already exists", code))
func AlreadyExists(field, message string) *DomainError {
	return &DomainError{
		Kind:    ErrAlreadyExists,
		Field:   field,
		Message: message,
	}
}

// InvalidState refuses an action the resource's current state does not
// allow, such as rotating a revoked key. It is a 409, not a 422: the input
// is fine, and the client must wait or change the resource, not fix the
// request.
//
//	return errs.InvalidState("cannot rotate a revoked key")
func InvalidState(message string) *DomainError {
	return &DomainError{
		Kind:    ErrInvalidState,
		Message: message,
	}
}

// Forbidden refuses an authenticated caller who may not do this. A service
// returns it for a rule that must hold on every route that reaches it. Return
// it only before any change: the idempotency layer does not store a 403, so
// another caller's request with the same key runs.
//
//	return errs.Forbidden("this key cannot do that")
func Forbidden(message string) *DomainError {
	return &DomainError{
		Kind:    ErrForbidden,
		Message: message,
	}
}

// NotFound is the error for a resource the request names that does not
// exist for the caller, such as the customer_id of a credit grant. Its
// message, "<resource> not found", names what was missing even when it is
// not the resource the route is named after. A row of another account or
// mode is reported the same way.
//
//	return errs.NotFound("customer")
func NotFound(resource string) *DomainError {
	return &DomainError{
		Kind:    ErrNotFound,
		Code:    "not_found",
		Message: resource + " not found",
	}
}

// Unavailable refuses a request because something it needed failed, such as
// a database read. The idempotency layer does not store the 503, so the
// client sends the same request again with the same key: return it only when
// nothing the request committed would be done twice. code names what was
// unavailable; cause goes to the log, never to the client.
//
//	return errs.Unavailable("proration_unavailable", "could not read this period's invoice — please retry", err)
func Unavailable(code, message string, cause error) *DomainError {
	return &DomainError{
		Kind:    ErrUnavailable,
		Code:    code,
		Message: message,
		Cause:   cause,
	}
}

// CodeEmailNotConfigured is the code of EmailNotConfigured.
const CodeEmailNotConfigured = "email_not_configured"

// EmailNotConfigured is the 409 an operator's email action returns on a
// server with no SMTP_HOST. Email is queued and sent later, so without it the
// action would say "sent" while the queued email failed unseen. Return it
// before anything that cannot be undone, such as minting a link.
func EmailNotConfigured() *DomainError {
	return InvalidState("Email isn't set up on this server, so nothing was sent.").WithCode(CodeEmailNotConfigured)
}

// CodeStripeNotConnected is the code of StripeNotConnected.
const CodeStripeNotConnected = "stripe_not_connected"

// StripeNotConnected is the 409 for an operator action that needs Stripe in
// a mode the account has not connected. A failure to read the keys is a
// server fault, not this.
func StripeNotConnected() *DomainError {
	return InvalidState("Stripe is not connected for this mode. Connect it under Settings, then try again.").WithCode(CodeStripeNotConnected)
}

// CodeReturnAddressNotSet is the code of ReturnAddressNotSet.
const CodeReturnAddressNotSet = "return_address_not_set"

// ReturnAddressNotSet is the 409 for an operator action that would send a
// customer to Stripe from a server with no address for Stripe to send them
// back to. It is returned before any Stripe call. setting names the server
// setting that holds the address, such as CUSTOMER_PORTAL_URL.
func ReturnAddressNotSet(setting string) *DomainError {
	return InvalidState(setting + " isn't set on this server, so Stripe has no page to send the customer back to. Set it to your dashboard's address, then restart.").WithCode(CodeReturnAddressNotSet)
}

// CodeOnlinePaymentUnavailable is the customer-facing code for the same
// states on a public page (hosted invoice Pay, update payment method): the
// seller has not connected Stripe for the mode, or this server has no
// address for Stripe to send the customer back to. The page explains it in
// its own words, and the code names no setting the customer could change.
const CodeOnlinePaymentUnavailable = "online_payment_unavailable"

// The 409 codes of a plan price write (ADR-130 §4, §16). price_exists: the
// plan already has a price on the meter with the same dimension_match.
// price_conflict: another price on the plan and meter could take the same
// events with the same number of labels, so neither would win. meter_overlap:
// the price would add a meter to a plan whose subscriber already has that
// meter on another subscription.
const (
	CodePriceExists   = "price_exists"
	CodePriceConflict = "price_conflict"
	CodeMeterOverlap  = "meter_overlap"
)

// CodeMeterHasUsage is the 409 code for a change to a meter's aggregation
// once the meter has events (ADR-130 §2): its usage so far was counted
// under the aggregation it has.
const CodeMeterHasUsage = "meter_has_usage"

// The kinds. A store or service returns one, usually through a constructor
// above, and respond.FromError picks the status from it.
var (
	ErrNotFound      = errors.New("not found")
	ErrAlreadyExists = errors.New("already exists")
	ErrInvalidState  = errors.New("invalid state")
	ErrForbidden     = errors.New("forbidden")
	// ErrValidation is bad input. Required and Invalid also set the field,
	// which the client shows the message under.
	ErrValidation  = errors.New("validation error")
	ErrUnavailable = errors.New("unavailable")
)

// CodeInvoiceBillingPeriodTaken and CodeInvoiceThresholdCycleTaken name the
// unique index an invoice insert hit (the invoice store's
// mapInvoiceUniqueViolation): the subscription's period slot
// (idx_invoices_billing_period_dedup) and its cycle's threshold slot
// (idx_invoices_threshold_unique_per_cycle). Billing branches on them and
// may not import the invoice package.
const (
	CodeInvoiceBillingPeriodTaken  = "invoice_billing_period_taken"
	CodeInvoiceThresholdCycleTaken = "invoice_threshold_cycle_taken"
)
