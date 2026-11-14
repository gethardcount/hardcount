package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/session"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// eventStreamRoute mounts GET /v1/webhook_events/stream: the session cookie
// or API key (session.MiddlewareOrAPIKey), the rate limit, the apikey:read
// permission, then the stream, which re-checks its credential every interval
// (streamCredentialStopped). router.go passes webhook.StreamHeartbeatInterval.
func eventStreamRoute(h *webhook.Handler, sessSvc *session.Service, authSvc *auth.Service, rateLimit func(http.Handler) http.Handler, every time.Duration) func(chi.Router) {
	return func(r chi.Router) {
		r.Use(session.MiddlewareOrAPIKey(sessSvc, authSvc))
		r.Use(rateLimit)
		r.With(auth.Require(auth.PermAPIKeyRead)).Get("/", h.StreamHandler(streamCredentialStopped(sessSvc, authSvc), every))
	}
}

// streamCredentialStopped is an open stream's re-check of its credential: it
// runs session.Authenticate on the stream's request. Two answers are verdicts
// that the credential stopped working: session.ErrNoCredentials (the session
// ended and the request sends no key) and auth.ErrInvalidKey (the key was
// revoked or has expired). Any other error is no verdict, a store failure,
// and is returned for the stream to log.
func streamCredentialStopped(sessSvc *session.Service, authSvc *auth.Service) func(*http.Request) (bool, error) {
	return func(r *http.Request) (bool, error) {
		_, err := session.Authenticate(r, sessSvc, authSvc)
		switch {
		case err == nil:
			return false, nil
		case errors.Is(err, session.ErrNoCredentials), errors.Is(err, auth.ErrInvalidKey):
			return true, nil
		default:
			return false, err
		}
	}
}
