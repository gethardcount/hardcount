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

// Authenticate resolves r's credentials to a context that carries the caller
// (auth.CallerOf) and its mode. A live session cookie wins over any key; a
// cookie that does not resolve falls through to the Bearer key. It returns
// ErrNoCredentials when there is neither, ValidateKey's error for a bad key,
// and an unavailableError when a store fails.
func Authenticate(r *http.Request, sessSvc *Service, keySvc *auth.Service) (context.Context, error) {
	ctx := r.Context()
	if c, err := r.Cookie(CookieName); err == nil && c.Value != "" {
		sess, err := sessSvc.Resolve(ctx, c.Value)
		if err == nil {
			return applyToCtx(ctx, sess), nil
		}
		if !errors.Is(err, ErrNotFound) {
			return nil, unavailableError{msg: "session resolve unavailable", err: err}
		}
	}

	rawKey := bearerToken(r)
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
	ctx = auth.WithCaller(ctx, auth.Caller{TenantID: key.TenantID, KeyID: key.ID, KeyType: auth.KeyType(key.KeyType)})
	return postgres.WithLivemode(ctx, key.Livemode), nil
}

// RequireCaller authenticates each request (Authenticate) and answers what it
// cannot: 401 with no credentials or a key that is not valid, and a retryable
// 503 when a store failed. The dashboard uses the cookie, and SDKs and curl
// callers use the API key.
func RequireCaller(sessSvc *Service, keySvc *auth.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return CallerRequired{sessions: sessSvc, keys: keySvc, next: next}
	}
}

// CallerRequired authenticates each request (Authenticate) and answers what it
// cannot: 401 with no credentials or a key that is not valid, and a retryable
// 503 when a store failed. RequireCaller makes it.
type CallerRequired struct {
	sessions *Service
	keys     *auth.Service
	next     http.Handler
}

func (c CallerRequired) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, err := Authenticate(r, c.sessions, c.keys)
	var unavailable unavailableError
	switch {
	case err == nil:
		// The access log wraps this middleware and cannot see ctx.
		request.SetCaller(ctx, callerIn(ctx))
		c.next.ServeHTTP(w, r.WithContext(ctx))
	case errors.Is(err, ErrNoCredentials):
		respond.Unauthorized(w, r, "missing credentials — sign in at /login or send Authorization: Bearer hc_secret_...")
	case errors.As(err, &unavailable):
		slog.Log(r.Context(), logging.ErrorLevel(r.Context(), unavailable.err), unavailable.msg, "error", unavailable.err)
		respond.Error(w, r, http.StatusServiceUnavailable,
			"authentication_unavailable",
			"authentication is temporarily unavailable — retry shortly")
	default:
		slog.WarnContext(r.Context(), "api key validation failed", "error", err)
		respond.Unauthorized(w, r, "invalid or expired API key")
	}
}

// callerIn names the caller for the access log with audit.ResolveActor's
// names; session cannot import audit. TestCallerIn_APIKey keeps them equal.
func callerIn(ctx context.Context) request.Caller {
	c := auth.CallerOf(ctx)
	if c.UserID != "" {
		return request.Caller{ActorType: "user", ActorID: c.UserID, TenantID: c.TenantID}
	}
	return request.Caller{ActorType: "api_key", ActorID: c.KeyID, TenantID: c.TenantID}
}

// NameCaller names s's user on the request's record, for the access log. It
// is for a handler that resolves the session cookie itself (sign-out and the
// mode switch); RequireCaller names the caller of every route behind it.
func NameCaller(ctx context.Context, s Session) {
	request.SetCaller(ctx, request.Caller{ActorType: "user", ActorID: s.UserID, TenantID: s.TenantID})
}

// applyToCtx returns ctx carrying s's user as the caller, in s's mode.
func applyToCtx(ctx context.Context, s Session) context.Context {
	ctx = auth.WithCaller(ctx, auth.Caller{TenantID: s.TenantID, UserID: s.UserID})
	return postgres.WithLivemode(ctx, s.Livemode)
}

// bearerToken returns the API key r carries: the Authorization Bearer token,
// or "". Nothing else reads a key's value from a request, and Authenticate is
// its only caller.
func bearerToken(r *http.Request) string {
	if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(token)
	}
	return ""
}
