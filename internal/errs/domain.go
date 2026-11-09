package errs

import (
	"errors"
	"fmt"
)

// DomainError is a structured error. It carries machine-readable metadata
// (Kind for HTTP status routing, Code for business-rule codes, Field for the
// offending form input) and a human-readable Message. The API layer turns it
// into an HTTP response by matching on Kind first. It then puts Field on the
// response envelope, so the frontend can show the message at the right
// input. Cause is optional and used only for internal and log context.
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

// WithCode attaches a stable, machine-readable error code to a DomainError.
// Use it to add a domain-specific code (such as "invoice_number_taken") on
// top of the generic Kind routing. It is safe to chain onto any constructor:
//
//	return errs.AlreadyExists("invoice_number", "invoice number taken").WithCode("invoice_number_taken")
//
// Promised codes are part of the public API, because integrators switch on
// them. Change the code string only when the underlying failure semantics
// change.
func (e *DomainError) WithCode(code string) *DomainError {
	e.Code = code
	return e
}

// Is lets errors.Is match against this error's Kind sentinel. This is how
// respond.FromError routes DomainError{Kind: ErrValidation} to a 422 response
// without the caller needing to wrap manually.
func (e *DomainError) Is(target error) bool {
	if e.Kind == nil {
		return false
	}
	return e.Kind == target
}

// New returns a DomainError with code and message, and no Kind.
func New(code, message string) *DomainError {
	return &DomainError{Code: code, Message: message}
}

// Wrap returns a DomainError with code and message that wraps cause.
func Wrap(cause error, code, message string) *DomainError {
	return &DomainError{Code: code, Message: message, Cause: cause}
}

// Code returns the Code of a DomainError anywhere in the error chain, or "".
func Code(err error) string {
	var de *DomainError
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

// Field extracts the offending field name from a DomainError anywhere in the
// error chain, or returns "". Used by respond.FromError to populate
// ErrorDetail.Param on the response envelope.
func Field(err error) string {
	var de *DomainError
	if errors.As(err, &de) {
		return de.Field
	}
	return ""
}

// Required returns a validation error indicating a required field is missing.
// Maps to HTTP 422 with Param=field so the client can highlight the input.
//
//	return errs.Required("external_id")
func Required(field string) *DomainError {
	return &DomainError{
		Kind:    ErrValidation,
		Field:   field,
		Message: field + " is required",
	}
}

// Invalid returns a validation error for a field that was provided but failed
// a semantic check (a bad format, out of range and so on). The reason is
// shown to the user verbatim. Phrase it as a complete sentence fragment that
// reads naturally under the field, such as "must be a valid email address".
//
//	return errs.Invalid("company_email", "must contain @ and a domain")
func Invalid(field, reason string) *DomainError {
	return &DomainError{
		Kind:    ErrValidation,
		Field:   field,
		Message: reason,
	}
}

// AlreadyExists marks a duplicate resource, optionally naming the offending
// field. Maps to HTTP 409. Use when a user-submitted value collides with an
// existing record (customer.external_id, plan.code, etc.).
//
//	return errs.AlreadyExists("code", fmt.Sprintf("plan code %q already exists", code))
func AlreadyExists(field, message string) *DomainError {
	return &DomainError{
		Kind:    ErrAlreadyExists,
		Field:   field,
		Message: message,
	}
}

// InvalidState rejects an operation because the target resource is in a
// state that disallows it (such as "can only activate draft subscriptions").
// It maps to HTTP 409, because the input is fine and the resource's state is
// the conflict. A 422 would mean "fix your input" (see respond/errors.go).
// These errors are not about a specific form field. The user usually needs a
// toast or an inline banner, not a field highlight.
//
//	return errs.InvalidState("can only activate draft subscriptions")
func InvalidState(message string) *DomainError {
	return &DomainError{
		Kind:    ErrInvalidState,
		Message: message,
	}
}

// Forbidden marks an authorization denial: the caller is authenticated but
// not permitted to perform this action. It differs from Invalid, which is bad
// input, and from a 401 for missing authentication. It maps to HTTP 403. Use
// it in the service layer when an authorization rule must hold whichever
// handler or route the call came through, so no future caller can bypass
// the rule.
//
//	return errs.Forbidden("this key cannot do that")
func Forbidden(message string) *DomainError {
	return &DomainError{
		Kind:    ErrForbidden,
		Message: message,
	}
}

// NotFound marks a referenced resource that does not exist for the caller.
// Maps to HTTP 404 with the message "<resource> not found". Use it when the
// missing thing is not the resource the endpoint is named after (a
// customer_id in the body of a credit grant), so the response names what was
// actually missing. A row hidden by tenant or mode isolation reports the same
// way as one that does not exist.
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
// a database read. It maps to HTTP 503 with Retry-After, and the idempotency
// layer does not store a 503, so the client sends the same request again
// with the same key. Return it only when a second run is safe: nothing the
// request committed would be done twice. code names what was unavailable.
// cause goes to the server log, never to the client.
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

// CodeEmailNotConfigured is the stable code an operator-initiated email
// action answers with when this server has no SMTP_HOST. Sends are queued
// and delivered later. Without this refusal, the action would report "sent"
// while the queued row failed with "SMTP not configured", unseen by the
// operator.
const CodeEmailNotConfigured = "email_not_configured"

// EmailNotConfigured is the 409 every operator email action returns when the
// server cannot send email. Callers return it before doing anything that
// cannot be undone (minting a link, creating an invitation).
func EmailNotConfigured() *DomainError {
	return InvalidState("Email isn't set up on this server, so nothing was sent.").WithCode(CodeEmailNotConfigured)
}

// CodeStripeNotConnected is the stable code an operator action that needs
// Stripe answers with when the account has no Stripe keys for the mode.
const CodeStripeNotConnected = "stripe_not_connected"

// StripeNotConnected is the 409 for an operator action that needs Stripe in
// a mode the account has not connected. It is the caller's state to fix, so
// it is not a 500. A failure to read the credentials is a different thing
// and stays an error (a 500).
func StripeNotConnected() *DomainError {
	return InvalidState("Stripe is not connected for this mode. Connect it under Settings, then try again.").WithCode(CodeStripeNotConnected)
}

// CodeReturnAddressNotSet is the stable code an operator action that sends a
// customer to Stripe answers with when this server has no address for Stripe
// to send the customer back to. Without the address there is no page that
// works, so the action is refused before any Stripe call.
const CodeReturnAddressNotSet = "return_address_not_set"

// ReturnAddressNotSet is the 409 for that state. setting names the server
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

// Sentinel errors for store and service layers.
var (
	ErrNotFound           = errors.New("not found")
	ErrAlreadyExists      = errors.New("already exists")
	ErrDuplicateKey       = errors.New("duplicate key")
	ErrInvalidState       = errors.New("invalid state")
	ErrPreconditionFailed = errors.New("precondition failed")
	// ErrForbidden marks an authorization denial (authenticated but not
	// permitted). respond.FromError maps it to HTTP 403. Prefer
	// errs.Forbidden.
	ErrForbidden = errors.New("forbidden")
	// ErrValidation marks an error caused by bad caller input (a missing
	// field, a malformed value). New code should prefer errs.Required,
	// errs.Invalid or errs.AlreadyExists, which set the Field metadata the UI
	// uses. The older wrapper form still works:
	//
	//   return fmt.Errorf("%w: customer_id is required", errs.ErrValidation)
	//
	// But it leaves the frontend with only a toast, and no field highlight.
	ErrValidation = errors.New("validation error")
	// ErrUnavailable marks a request refused because something it needed
	// failed, when a second run of it is safe. respond.FromError maps it to
	// HTTP 503. Use errs.Unavailable, which carries the code and the cause.
	ErrUnavailable = errors.New("unavailable")
)

// Error code constants.
const (
	CodeBillingSetupIncomplete = "billing_setup_incomplete"
	CodeInvoiceFetchFailed     = "invoice_fetch_failed"
	CodePaymentRetryFailed     = "payment_retry_failed"
	CodeSubscriptionInvalid    = "subscription_invalid"
	CodePricingUnsupported     = "pricing_unsupported"
	CodeUsageSyncFailed        = "usage_sync_failed"
	CodeAdapterNotConfigured   = "adapter_not_configured"
)
