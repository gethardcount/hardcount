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
	"github.com/gethardcount/hardcount/internal/platform/request"
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

// Authenticate resolves r's credentials to the caller's context. A session
// cookie that resolves is the caller, whatever key the request also carries.
// A cookie that does not resolve (an ended or unknown session) falls through
// to the API key (apiKeyFromHeaders), so a stale cookie does not block a valid
// key on the same request; with no key, Authenticate returns
// ErrNoCredentials, as it does when the request carries neither. A key's
// error is returned as ValidateKey gave it: auth.ErrInvalidKey, wrapped, for
// a key that is not valid. A store failure, for the cookie or the key,
// returns an unavailableError: no verdict on the credential.
//
// The context names one caller: its tenant, and the session's user or the
// key, the other set to "", whatever r's context carried before. The event
// stream re-runs Authenticate on its own request and compares the two.
func Authenticate(r *http.Request, sessSvc *Service, keySvc *auth.Service) (context.Context, error) {
	ctx := r.Context()
	if c, err := r.Cookie(CookieName); err == nil && c.Value != "" {
		sess, err := sessSvc.Resolve(ctx, c.Value)
		if err == nil {
			return applyToCtx(ctx, sess), nil
		}
		if !errors.Is(err, ErrNotFound) {
			// A store failure is no verdict on the cookie, so it does not
			// fall through to "missing credentials".
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
		// A store failure, not a verdict on the credential.
		return nil, unavailableError{msg: "api key validation unavailable", err: err}
	}
	ctx = auth.WithTenantID(ctx, key.TenantID)
	ctx = auth.WithUserID(ctx, "")
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
				// The access log wraps this middleware and cannot see ctx.
				request.SetCaller(ctx, callerIn(ctx))
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

// callerIn names the one caller Authenticate's context carries: a session's
// user or an API key. It repeats audit.ResolveActor's names for the two,
// because session may not import audit (internal/arch/boundaries_test.go);
// TestCallerIn_APIKey and TestMiddlewareOrAPIKey_NamesTheCallerOnTheRecord
// fail if the two differ.
func callerIn(ctx context.Context) request.Caller {
	c := request.Caller{ActorType: "api_key", ActorID: auth.KeyID(ctx), TenantID: auth.TenantID(ctx)}
	if userID := auth.UserID(ctx); userID != "" {
		c.ActorType, c.ActorID = "user", userID
	}
	return c
}

// NameCaller names s's user on the request's record, for the access log. It
// is for a handler that resolves the session cookie itself (sign-out and the
// mode switch); MiddlewareOrAPIKey names the caller of every route behind it.
func NameCaller(ctx context.Context, s Session) {
	request.SetCaller(ctx, request.Caller{ActorType: "user", ActorID: s.UserID, TenantID: s.TenantID})
}

func applyToCtx(ctx context.Context, s Session) context.Context {
	ctx = auth.WithTenantID(ctx, s.TenantID)
	ctx = auth.WithUserID(ctx, s.UserID)
	ctx = auth.WithKeyID(ctx, "")
	// A session holds the KeyTypeSession permissions: roles are recorded but
	// not enforced (ADR-081).
	ctx = auth.WithKeyType(ctx, auth.KeyTypeSession)
	ctx = postgres.WithLivemode(ctx, s.Livemode)
	return ctx
}

// apiKeyFromHeaders returns the API key r carries: the Authorization Bearer
// token, or "". It is the one place a key is read from a request.
func apiKeyFromHeaders(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}
