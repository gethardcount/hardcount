package session

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/platform/logging"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// ErrNoCredentials means the request carries neither a session cookie that
// resolves nor an API key.
var ErrNoCredentials = errors.New("no credentials")

// unavailableError means authentication reached no verdict: a store failed.
// msg is what the middleware logs.
type unavailableError struct {
	msg string
	err error
}

func (e unavailableError) Error() string { return e.msg + ": " + e.err.Error() }
func (e unavailableError) Unwrap() error { return e.err }

// Authenticate resolves r's credentials to the principal's context. The
// session cookie comes first and takes precedence when both are present, so a
// browser tab with a stale Authorization header cannot bypass session
// revocation by accident. A cookie that is present but not valid (an ended or
// unknown session) falls through to the API key (apiKeyFromHeaders), so a
// stale cookie does not block a valid key on the same request; with no key,
// Authenticate then returns ErrNoCredentials, as it does when the request
// carries neither. A key's error is returned as ValidateKey gave it:
// auth.ErrInvalidKey, wrapped, for a key that is not valid. A store failure,
// for the cookie or the key, returns an unavailableError: no verdict on the
// credential.
func Authenticate(r *http.Request, sessSvc *Service, keySvc *auth.Service) (context.Context, error) {
	ctx := r.Context()
	if c, err := r.Cookie(CookieName); err == nil && c.Value != "" {
		sess, err := sessSvc.Resolve(ctx, c.Value)
		if err == nil {
			return applyToCtx(ctx, sess), nil
		}
		if !errors.Is(err, ErrNotFound) {
			// A store failure is no verdict on the cookie, so it does not
			// fall through to "missing credentials" (#560).
			return nil, unavailableError{msg: "session resolve unavailable", err: err}
		}
		// The cookie is present but invalid. Fall through to the API key, so
		// a stale cookie does not block a valid key on the same request.
	}

	rawKey := apiKeyFromHeaders(r)
	if rawKey == "" {
		return nil, ErrNoCredentials
	}
	key, err := keySvc.ValidateKey(ctx, rawKey)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidKey) {
			return nil, err
		}
		// An infrastructure failure, not a verdict on the credential (#560).
		return nil, unavailableError{msg: "api key validation unavailable", err: err}
	}
	ctx = auth.WithTenantID(ctx, key.TenantID)
	ctx = auth.WithKeyID(ctx, key.ID)
	ctx = auth.WithKeyType(ctx, auth.KeyType(key.KeyType))
	ctx = postgres.WithLivemode(ctx, key.Livemode)
	return ctx, nil
}

// MiddlewareOrAPIKey authenticates each request (Authenticate) and answers
// what it cannot: 401 with no credentials or a key that is not valid, and a
// retryable 503 when a store failed. The dashboard uses the cookie, and SDKs
// and curl callers use the API key.
func MiddlewareOrAPIKey(sessSvc *Service, keySvc *auth.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, err := Authenticate(r, sessSvc, keySvc)
			var unavailable unavailableError
			switch {
			case err == nil:
				next.ServeHTTP(w, r.WithContext(ctx))
			case errors.Is(err, ErrNoCredentials):
				respond.Unauthorized(w, r, "missing credentials — sign in at /login or send Authorization: Bearer hc_secret_...")
			case errors.As(err, &unavailable):
				slog.Log(r.Context(), logging.ErrorLevel(r.Context(), unavailable.err), unavailable.msg, "error", unavailable.err)
				respond.Error(w, r, http.StatusServiceUnavailable, "api_error",
					"authentication_unavailable",
					"authentication is temporarily unavailable — retry shortly")
			default:
				// Use a generic message that never says whether the key
				// exists, has expired or was revoked (ADR-026).
				slog.WarnContext(r.Context(), "api key validation failed", "error", err)
				respond.Unauthorized(w, r, "invalid or expired API key")
			}
		})
	}
}

func applyToCtx(ctx context.Context, s Session) context.Context {
	ctx = auth.WithTenantID(ctx, s.TenantID)
	ctx = auth.WithUserID(ctx, s.UserID)
	// Requests on a session use the KeyTypeSession permission set (full
	// operator access). Roles are recorded but not enforced (ADR-081):
	// every member, the owner or an invited 'member', gets the same full
	// set. When roles diverge, resolve the user's role from the
	// user_tenants row here and pick a permission set per role.
	ctx = auth.WithKeyType(ctx, auth.KeyTypeSession)
	ctx = postgres.WithLivemode(ctx, s.Livemode)
	return ctx
}

// apiKeyFromHeaders returns the API key r carries: the Authorization Bearer
// token, or else the X-API-Key header. It is the one place a key is read
// from a request.
func apiKeyFromHeaders(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return r.Header.Get("X-API-Key")
}
