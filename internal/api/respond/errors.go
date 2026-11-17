package respond

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/logging"
)

// operatorCodeError is a SafeMessageError that names its own code in place of
// provider_error, such as payment_outcome_unknown when the provider never
// answered and the charge may have gone through. "" keeps provider_error.
type operatorCodeError interface {
	OperatorErrorCode() string
}

// FromError writes the answer for an error a service or store returned. It
// picks the status from the error's kind, never from its text:
//
//	kind                   status  code (a DomainError's Code replaces it)   message
//	errs.ErrUnavailable    503     unavailable, and Retry-After: 1           the DomainError's own
//	errs.ErrNotFound       404     not_found                                 "<resource> not found", or the DomainError's own if it has a Code
//	errs.ErrAlreadyExists  409     already_exists                            err.Error()
//	errs.ErrInvalidState   409     invalid_state                             err.Error()
//	errs.ErrForbidden      403     forbidden (a Code is not used)            err.Error()
//	errs.ErrValidation     422     validation_error                          err.Error()
//	errs.SafeMessageError  502     provider_error, or its OperatorErrorCode  its OperatorSafeMessage()
//	anything else          500     internal_error                            a generic message
//
// A 409 or 422 has the DomainError's Field as its param. The 503, 502 and
// 500 answers log err; the others are the client's to fix and log nothing.
// A nil err writes nothing.
func FromError(w http.ResponseWriter, r *http.Request, err error, resource string) {
	if err == nil {
		return
	}
	field := errs.Field(err)
	code := errs.Code(err)

	switch {
	case errors.Is(err, errs.ErrUnavailable):
		// First, so a not-found in its cause cannot turn a refusal the client
		// should retry into a 404.
		unavailable(w, r, err, code, resource)

	case errors.Is(err, errs.ErrNotFound):
		if code != "" {
			Error(w, r, http.StatusNotFound, code, domainMessage(err, err.Error()))
		} else {
			NotFound(w, r, resource)
		}

	case errors.Is(err, errs.ErrAlreadyExists):
		ConflictCoded(w, r, field, code, err.Error())

	case errors.Is(err, errs.ErrInvalidState):
		if code == "" {
			code = "invalid_state"
		}
		writeError(w, r, http.StatusConflict, code, field, err.Error())

	case errors.Is(err, errs.ErrForbidden):
		Forbidden(w, r, err.Error())

	case errors.Is(err, errs.ErrValidation):
		ValidationCoded(w, r, field, code, err.Error())

	default:
		var sme errs.SafeMessageError
		if errors.As(err, &sme) {
			slog.Log(r.Context(), logging.ErrorLevel(r.Context(), err), "safe-message error returned",
				"error", err,
				"resource", resource,
			)
			code := "provider_error"
			var oc operatorCodeError
			if errors.As(err, &oc) && oc.OperatorErrorCode() != "" {
				code = oc.OperatorErrorCode()
			}
			Error(w, r, http.StatusBadGateway, code, sme.OperatorSafeMessage())
			return
		}

		// The body says nothing of err: support finds it in the log by the
		// request ID.
		slog.Log(r.Context(), logging.ErrorLevel(r.Context(), err), "unhandled error",
			"error", err,
			"resource", resource,
		)
		InternalError(w, r)
	}
}

// unavailable answers an errs.Unavailable refusal. Its cause is a raw server
// error, so it goes to the log and the body carries the DomainError's own
// message.
func unavailable(w http.ResponseWriter, r *http.Request, err error, code, resource string) {
	slog.Log(r.Context(), logging.ErrorLevel(r.Context(), err), "request refused as unavailable; the client may send it again",
		"error", err,
		"resource", resource,
	)
	if code == "" {
		code = "unavailable"
	}
	w.Header().Set("Retry-After", "1")
	Error(w, r, http.StatusServiceUnavailable, code, domainMessage(err, "the request could not be completed — please retry"))
}

// domainMessage is the Message of the DomainError in err's chain, without
// the prefixes its callers added, which name internal steps and ids. It is
// otherwise when the chain has no DomainError with a message.
func domainMessage(err error, otherwise string) string {
	var de *errs.DomainError
	if errors.As(err, &de) && de.Message != "" {
		return de.Message
	}
	return otherwise
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
