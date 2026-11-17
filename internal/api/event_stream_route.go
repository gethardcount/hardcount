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

// eventStreamRoute mounts GET /v1/webhook_events/stream behind the
// apikey:read permission; the /v1 group in router.go authenticates the caller
// and rate-limits first. Every interval every the stream checks its caller
// again (streamCredentialStopped): once the key is revoked or expires, or the
// session ends, it closes with no final frame, and a reconnect with the same
// credential answers 401. A store failure does not close it. router.go passes
// webhook.StreamHeartbeatInterval; it is a function, not inline in router.go,
// so the stream's credential test can mount it with a short interval.
func eventStreamRoute(h *webhook.Handler, sessSvc *session.Service, authSvc *auth.Service, every time.Duration) func(chi.Router) {
	return func(r chi.Router) {
		r.With(auth.Require(auth.PermAPIKeyRead)).Get("/", h.StreamHandler(streamCredentialStopped(sessSvc, authSvc), every))
	}
}

// streamCredentialStopped is an open stream's re-check of its caller: it runs
// session.Authenticate on the stream's request, and the stream goes on only
// while the answer is the caller that opened it, the same tenant and the same
// session user or key. A request can carry a cookie and a key: once the
// session ends the key answers, and it can be another caller, in another
// tenant. session.ErrNoCredentials (the session ended and no key was sent)
// and auth.ErrInvalidKey (the key was revoked or has expired) are verdicts
// too. Any other error is no verdict, a store failure, and is returned for
// the stream to log.
func streamCredentialStopped(sessSvc *session.Service, authSvc *auth.Service) func(*http.Request) (bool, error) {
	return func(r *http.Request) (bool, error) {
		ctx, err := session.Authenticate(r, sessSvc, authSvc)
		switch {
		case err == nil:
			return auth.CallerOf(ctx) != auth.CallerOf(r.Context()), nil
		case errors.Is(err, session.ErrNoCredentials), errors.Is(err, auth.ErrInvalidKey):
			return true, nil
		default:
			return false, err
		}
	}
}
