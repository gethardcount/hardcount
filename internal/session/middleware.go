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
// (auth.CallerOf) and its test or live mode. A live session cookie wins over
// any key; a cookie that does not resolve falls through to the Bearer key. It
// returns ErrNoCredentials when there is neither, ValidateKey's error for a
// bad key, and an unavailableError when a store fails.
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

	rawKey, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	rawKey = strings.TrimSpace(rawKey)
	if !ok || rawKey == "" {
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

// RequireCaller authenticates each request (Authenticate) and passes it on
// with the caller and its mode on the context. It answers 401 to a request
// with no credentials or a key that is not valid, and a retryable 503 when a
// store failed. The dashboard uses the cookie, and SDKs and curl callers use
// the API key.
func RequireCaller(sessSvc *Service, keySvc *auth.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return CallerRequired{sessions: sessSvc, keys: keySvc, next: next}
	}
}

// CallerRequired authenticates each request. RequireCaller makes it; its doc
// has the rule.
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
		request.SetActor(ctx, actorOf(auth.CallerOf(ctx)))
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

// NameActor names s's user on the request's record, for the access log. It
// is for a handler that resolves the session cookie itself (sign-out and the
// mode switch); RequireCaller names the caller of every route behind it.
func NameActor(ctx context.Context, s Session) {
	request.SetActor(ctx, actorOf(sessionCaller(s)))
}

// actorOf names c for the access log, in the audit log's words
// (auth.Caller.Actor).
func actorOf(c auth.Caller) request.Actor {
	actorType, id := c.Actor()
	return request.Actor{Type: actorType, ID: id, TenantID: c.TenantID}
}

// sessionCaller is s's user as a caller.
func sessionCaller(s Session) auth.Caller {
	return auth.Caller{TenantID: s.TenantID, UserID: s.UserID}
}

// applyToCtx returns ctx carrying s's user as the caller, in s's test or live
// mode.
func applyToCtx(ctx context.Context, s Session) context.Context {
	return postgres.WithLivemode(auth.WithCaller(ctx, sessionCaller(s)), s.Livemode)
}
