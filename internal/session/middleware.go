package session

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// Middleware accepts a `hardcount_session` httpOnly cookie. On hit it
// resolves the session, projects the parent key's tenant context onto
// the request, and forwards. On miss it 401s without falling back to
// API-key auth — use MiddlewareOrAPIKey for routes that should accept
// either credential.
func Middleware(svc *Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := r.Cookie(CookieName)
			if err != nil || c.Value == "" {
				respond.Unauthorized(w, r, "missing session — sign in at /login")
				return
			}
			sess, err := svc.Resolve(r.Context(), c.Value)
			if err != nil {
				if !errors.Is(err, ErrNotFound) {
					// Store failure, not a session verdict — surface as
					// retryable 5xx rather than a credential lie (#560).
					slog.ErrorContext(r.Context(), "session resolve unavailable", "error", err)
					respond.Error(w, r, http.StatusServiceUnavailable, "api_error",
						"authentication_unavailable",
						"authentication is temporarily unavailable — retry shortly")
					return
				}
				respond.Unauthorized(w, r, "invalid or expired session")
				return
			}
			next.ServeHTTP(w, r.WithContext(applyToCtx(r.Context(), sess)))
		})
	}
}

// MiddlewareOrAPIKey accepts either a session cookie OR an
// `Authorization: Bearer <api_key>` header. The dashboard rides the
// cookie path; SDKs and curl callers ride the API-key path. Cookie
// takes precedence when both are present so a browser tab with a
// stale Authorization header doesn't accidentally bypass session
// revocation.
func MiddlewareOrAPIKey(sessSvc *Service, keySvc *auth.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c, err := r.Cookie(CookieName); err == nil && c.Value != "" {
				sess, rerr := sessSvc.Resolve(r.Context(), c.Value)
				if rerr == nil {
					next.ServeHTTP(w, r.WithContext(applyToCtx(r.Context(), sess)))
					return
				}
				if !errors.Is(rerr, ErrNotFound) {
					// Store failure — no verdict on the cookie. Don't
					// degrade to "missing credentials"; surface the
					// outage as retryable (#560).
					slog.ErrorContext(r.Context(), "session resolve unavailable", "error", rerr)
					respond.Error(w, r, http.StatusServiceUnavailable, "api_error",
						"authentication_unavailable",
						"authentication is temporarily unavailable — retry shortly")
					return
				}
				// Cookie present but invalid — fall through to API-key
				// rather than 401, so a stale cookie doesn't block a
				// valid Bearer header on the same request.
			}

			rawKey := extractBearer(r)
			if rawKey == "" {
				respond.Unauthorized(w, r, "missing credentials — sign in at /login or send Authorization: Bearer hc_secret_...")
				return
			}
			key, err := keySvc.ValidateKey(r.Context(), rawKey)
			if err != nil {
				if !errors.Is(err, auth.ErrInvalidKey) {
					// Infra failure, not a credential verdict (#560).
					slog.ErrorContext(r.Context(), "api key validation unavailable", "error", err)
					respond.Error(w, r, http.StatusServiceUnavailable, "api_error",
						"authentication_unavailable",
						"authentication is temporarily unavailable — retry shortly")
					return
				}
				// Generic — never reveal whether the key exists, is
				// expired, or is revoked. ADR-026.
				slog.WarnContext(r.Context(), "api key validation failed",
					"error", err)
				respond.Unauthorized(w, r, "invalid or expired API key")
				return
			}
			ctx := r.Context()
			ctx = auth.WithTenantID(ctx, key.TenantID)
			ctx = auth.WithKeyID(ctx, key.ID)
			ctx = auth.WithKeyType(ctx, auth.KeyType(key.KeyType))
			ctx = postgres.WithLivemode(ctx, key.Livemode)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func applyToCtx(ctx context.Context, s Session) context.Context {
	ctx = auth.WithTenantID(ctx, s.TenantID)
	ctx = auth.WithUserID(ctx, s.UserID)
	// Session-authed requests use the KeyTypeSession permission set
	// (full operator access). Invites shipped (ADR-081) but roles are
	// recorded-not-enforced: every member — owner or invited 'member' —
	// gets the same full set. When roles diverge, resolve the user's
	// role from the user_tenants row here and pick a permission set per
	// role.
	ctx = auth.WithKeyType(ctx, auth.KeyTypeSession)
	ctx = postgres.WithLivemode(ctx, s.Livemode)
	return ctx
}

func extractBearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return r.Header.Get("X-API-Key")
}
