package respond

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/logging"
)

// SafeMessageError is an alias for errs.SafeMessageError, the opt-in marker
// for an error type that renders its own operator-safe message (ADR-026).
// The interface lives in internal/errs, so code outside HTTP, such as the
// test-clock catch-up orchestrator and scheduler error rollups, can use the
// same check. The alias keeps existing call sites such as
// `respond.FromError(... &MyErr{})` working without churn.
type SafeMessageError = errs.SafeMessageError

// OperatorCodeError lets a SafeMessageError name a more specific code than
// provider_error, so a caller can branch on it. For example,
// payment_outcome_unknown means the provider never answered, and the charge
// may have gone through. Returning "" keeps provider_error.
type OperatorCodeError interface {
	OperatorErrorCode() string
}

// FromError translates a service or store error into its HTTP response. It
// is the one place where domain errors become API responses.
//
// Usage in handlers:
//
//	customer, err := h.svc.Create(ctx, tenantID, input)
//	if err != nil {
//	    respond.FromError(w, r, err, "customer")
//	    return
//	}
func FromError(w http.ResponseWriter, r *http.Request, err error, resource string) {
	if err == nil {
		return
	}

	// field is the offending field of a DomainError in the chain, if there is
	// one. It is empty when the error came from a legacy fmt.Errorf site or a
	// wrapped sentinel.
	field := errs.Field(err)
	// code is a stable, domain-specific error code, such as
	// "invoice_number_taken". When it is set, it goes in the envelope's Code
	// slot instead of the generic category ("validation_error",
	// "already_exists"). Integrators switch on the domain code to give each
	// case its own user experience.
	code := errs.Code(err)

	switch {
	case errors.Is(err, errs.ErrUnavailable):
		// Checked first, so a not-found deeper in its cause cannot turn a
		// refusal the client should retry into a 404.
		unavailable(w, r, err, code, resource)

	case errors.Is(err, errs.ErrNotFound):
		if code != "" {
			NotFoundCoded(w, r, code, ownMessage(err))
		} else {
			NotFound(w, r, resource)
		}

	case errors.Is(err, errs.ErrAlreadyExists):
		ConflictCoded(w, r, field, code, err.Error())

	case errors.Is(err, errs.ErrInvalidState):
		// A state conflict is a 409, not a 422. The request body is fine on
		// its own. The conflict is with the resource's current state, such
		// as "clock is advancing, must be ready to advance" or "cannot
		// rotate a revoked key". Stripe, GitHub and AWS all do the same: 422
		// means "fix your input", and 409 means "wait for the operation in
		// flight, or change the resource state". The default code is
		// "invalid_state". An explicit DomainError code, such as
		// "clock_advancing", overrides it for clients that switch on a
		// specific reason.
		c := code
		if c == "" {
			c = "invalid_state"
		}
		errorField(w, r, http.StatusConflict, c, field, err.Error())

	case errors.Is(err, errs.ErrForbidden):
		// Authenticated but not permitted is a 403. The request is
		// well-formed and the resource state is fine, but the caller's
		// principal lacks authority for this action. A service enforces such
		// a rule itself, through errs.Forbidden.
		Forbidden(w, r, err.Error())

	case errors.Is(err, errs.ErrValidation):
		ValidationCoded(w, r, field, code, err.Error())

	default:
		// An error that opts into its own operator-safe rendering, such as a
		// PaymentError with a DeclineCode, has a curated message, so show it.
		// The status is 502 because the request reached an upstream provider
		// and the provider refused it.
		var sme SafeMessageError
		if errors.As(err, &sme) {
			slog.Log(r.Context(), logging.ErrorLevel(r.Context(), err), "safe-message error returned",
				"error", err,
				"resource", resource,
			)
			code := "provider_error"
			var oc OperatorCodeError
			if errors.As(err, &oc) && oc.OperatorErrorCode() != "" {
				code = oc.OperatorErrorCode()
			}
			Error(w, r, http.StatusBadGateway, code, sme.OperatorSafeMessage())
			return
		}

		// Any other error has no kind, so it is a server fault whatever its
		// text says, and a DomainError with a code but no kind is one too.
		// Log it and return a generic body: raw database, Stripe and Go
		// errors never reach operators. Support finds the full error in the
		// logs by the request ID in the response header.
		slog.Log(r.Context(), logging.ErrorLevel(r.Context(), err), "unhandled error",
			"error", err,
			"resource", resource,
		)
		InternalError(w, r)
	}
}

// unavailable answers an errs.Unavailable refusal: 503 with Retry-After: 1.
// The body carries the DomainError's own message, because its cause is a raw
// server error that is logged here and never shown.
func unavailable(w http.ResponseWriter, r *http.Request, err error, code, resource string) {
	slog.Log(r.Context(), logging.ErrorLevel(r.Context(), err), "request refused as unavailable; the client may send it again",
		"error", err,
		"resource", resource,
	)
	if code == "" {
		code = "unavailable"
	}
	message := "the request could not be completed — please retry"
	var de *errs.DomainError
	if errors.As(err, &de) && de.Message != "" {
		message = de.Message
	}
	w.Header().Set("Retry-After", "1")
	Error(w, r, http.StatusServiceUnavailable, code, message)
}

// ownMessage returns the message of the DomainError in err's chain, without
// the prefixes its callers added when they wrapped it: those name internal
// steps and ids. With no DomainError it returns err.Error().
func ownMessage(err error) string {
	var de *errs.DomainError
	if errors.As(err, &de) && de.Message != "" {
		return de.Message
	}
	return err.Error()
}

// routePattern returns the matched chi route pattern (for example
// "/v1/api-keys/{id}/rotate"), or "" when the request did not come through a
// chi router.
func routePattern(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		return rctx.RoutePattern()
	}
	return ""
}
