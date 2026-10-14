package respond

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/errs"
)

// SafeMessageError is an alias for errs.SafeMessageError — the
// opt-in marker for error types that own their own operator-safe
// rendering. The interface lives in internal/errs so non-HTTP
// contexts (catchup orchestrator, scheduler error rollups) can use
// the same dispatch shape; this alias preserves existing call sites
// like `respond.FromError(... &MyErr{})` without forcing a churn.
//
// ADR-026.
type SafeMessageError = errs.SafeMessageError

// OperatorCodeError lets a SafeMessageError name a more specific code than
// provider_error, so a caller can branch on it (payment_outcome_unknown:
// the provider never answered, the charge may have gone through).
// Returning "" keeps provider_error.
type OperatorCodeError interface {
	OperatorErrorCode() string
}

// FromError translates a service/store error into the appropriate HTTP response.
// This is the single place where domain errors become API responses.
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

	// Pull the offending field (if any) off a DomainError in the chain. Empty
	// when the error came from a legacy fmt.Errorf site or a sentinel wrap.
	field := errs.Field(err)
	// Stable, domain-specific error code (e.g. "coupon_expired"). When
	// non-empty, we plumb it into the envelope's Code slot instead of the
	// generic category ("validation_error", "already_exists"). Integrators
	// switch on the domain code for differential UX.
	code := errs.Code(err)

	switch {
	case errors.Is(err, errs.ErrNotFound):
		if code != "" {
			NotFoundCoded(w, r, code, err.Error())
		} else {
			NotFound(w, r, resource)
		}

	case errors.Is(err, errs.ErrAlreadyExists):
		ConflictCoded(w, r, field, code, err.Error())

	case errors.Is(err, errs.ErrDuplicateKey):
		ConflictCoded(w, r, field, code, err.Error())

	case errors.Is(err, errs.ErrPreconditionFailed):
		if code != "" {
			errorField(w, r, http.StatusPreconditionFailed, "invalid_request_error", code, "", err.Error())
		} else {
			PreconditionFailed(w, r, err.Error())
		}

	case errors.Is(err, errs.ErrInvalidState):
		// State conflicts get HTTP 409, not 422. The request body is
		// fine in isolation; the conflict is the resource's current
		// state (e.g. "clock is advancing, must be ready to advance",
		// "cannot rotate a revoked key"). Stripe / GitHub / AWS all
		// follow this — 422 means "fix your input", 409 means "wait
		// for the in-flight operation or change the resource state."
		// Default code is "invalid_state"; an explicit DomainError
		// code (e.g. "clock_advancing") overrides it for clients that
		// want to switch on a specific reason.
		c := code
		if c == "" {
			c = "invalid_state"
		}
		errorField(w, r, http.StatusConflict, "invalid_request_error", c, field, err.Error())

	case errors.Is(err, errs.ErrForbidden):
		// Authenticated but not permitted → 403. The request is well-formed
		// and the resource state is fine; the caller's principal simply lacks
		// authority for this action (e.g. minting a platform key without being
		// a platform principal).
		Forbidden(w, r, err.Error())

	case errors.Is(err, errs.ErrValidation):
		ValidationCoded(w, r, field, code, err.Error())

	default:
		// DomainError with an explicit code — treat as validation (these are
		// business-rule rejections like billing_setup_incomplete).
		if code != "" {
			ValidationCoded(w, r, field, code, err.Error())
			return
		}

		// Errors that opt into their own operator-safe rendering
		// (PaymentError with DeclineCode etc.). These have curated
		// messages — surface them. Status is 502 because the request
		// reached an upstream provider and the provider rejected it.
		var sme SafeMessageError
		if errors.As(err, &sme) {
			slog.ErrorContext(r.Context(), "safe-message error returned",
				"error", err,
				"resource", resource,
			)
			code := "provider_error"
			var oc OperatorCodeError
			if errors.As(err, &oc) && oc.OperatorErrorCode() != "" {
				code = oc.OperatorErrorCode()
			}
			Error(w, r, http.StatusBadGateway, "api_error", code, sme.OperatorSafeMessage())
			return
		}

		// Legacy fallback: services that still return fmt.Errorf("... is
		// required") without wrapping errs.ErrValidation. This heuristic is
		// intentionally conservative (anchored prefixes, not substring) so
		// DB errors containing "invalid" don't get mis-mapped to 422 with
		// their raw message leaked to the client.
		//
		// It still classifies by text, so a server fault whose message
		// happens to match is answered as a 422 carrying that message. The
		// warning makes every use visible, so the remaining sites can be
		// found and given typed errors. It logs the matched route pattern,
		// not the path: some public paths carry a token.
		//
		// TODO: migrate all service-layer validation sites to wrap
		// errs.ErrValidation, then delete this fallback. See
		// internal/errs/domain.go for the pattern.
		if looksLikeValidationError(err.Error()) {
			slog.WarnContext(r.Context(), "untyped error answered as 422 by message text",
				"error", err,
				"resource", resource,
				"method", r.Method,
				"route", routePattern(r),
			)
			Validation(w, r, err.Error())
			return
		}

		// Default: log full error server-side, return generic body.
		// Boundary sanitization — never leak raw DB / Stripe / Go
		// internals to operators. Request-ID in the response header
		// is the bridge for support to find the full trace in logs.
		slog.ErrorContext(r.Context(), "unhandled error",
			"error", err,
			"resource", resource,
		)
		InternalError(w, r)
	}
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

// looksLikeValidationError uses anchored prefixes (not substrings) to classify
// service-layer validation messages. The old substring match would catch DB
// errors like `pq: invalid input syntax for type uuid` and leak them as 422
// bodies. Prefix-anchoring keeps false positives to services that start their
// error message with these phrases — a narrow, intentional pattern.
func looksLikeValidationError(msg string) bool {
	prefixes := []string{
		"invalid ",
		"missing ",
		"cannot ",
		"can only ",
	}
	msgLower := strings.ToLower(msg)
	for _, p := range prefixes {
		if strings.HasPrefix(msgLower, p) {
			return true
		}
	}
	// Common "<field> is required" / "<field> must be ..." shape from Go services.
	substrings := []string{
		" is required",
		" must be ",
		" can only ",
		" at least ",
	}
	for _, s := range substrings {
		if strings.Contains(msgLower, s) {
			return true
		}
	}
	return false
}
