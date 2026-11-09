package session

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// MiddlewareOrAPIKey accepts either a session cookie or an
// `Authorization: Bearer <api_key>` header. The dashboard uses the cookie,
// and SDKs and curl callers use the API key. The cookie takes precedence
// when both are present, so a browser tab with a stale Authorization header
// cannot bypass session revocation by accident.
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
					// A store failure is no verdict on the cookie. Do not
					// fall back to "missing credentials": report the outage
					// as retryable (#560).
					slog.Log(r.Context(), respond.ErrorLevel(r.Context(), rerr), "session resolve unavailable", "error", rerr)
					respond.Error(w, r, http.StatusServiceUnavailable, "api_error",
						"authentication_unavailable",
						"authentication is temporarily unavailable — retry shortly")
					return
				}
				// The cookie is present but invalid. Fall through to the API
				// key instead of a 401, so a stale cookie does not block a
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
					// An infrastructure failure, not a verdict on the
					// credential (#560).
					slog.Log(r.Context(), respond.ErrorLevel(r.Context(), err), "api key validation unavailable", "error", err)
					respond.Error(w, r, http.StatusServiceUnavailable, "api_error",
						"authentication_unavailable",
						"authentication is temporarily unavailable — retry shortly")
					return
				}
				// Use a generic message that never says whether the key
				// exists, has expired or was revoked (ADR-026).
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
	// Requests on a session use the KeyTypeSession permission set (full
	// operator access). Roles are recorded but not enforced (ADR-081):
	// every member, the owner or an invited 'member', gets the same full
	// set. When roles diverge, resolve the user's role from the
	// user_tenants row here and pick a permission set per role.
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
