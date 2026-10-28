package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

type contextKey string

const (
	tenantIDKey      contextKey = "tenant_id"
	apiKeyIDKey      contextKey = "api_key_id"
	userIDKey        contextKey = "user_id"
	keyTypeKey       contextKey = "key_type"
	customerActorKey contextKey = "customer_actor_id"
)

// TestTenantIDKey returns the context key for tenant ID (for use in tests).
func TestTenantIDKey() contextKey { return tenantIDKey }

// Middleware validates API key credentials and injects tenant context.
func Middleware(svc *Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rawKey := extractBearerToken(r)
			if rawKey == "" {
				respond.Unauthorized(w, r, "missing credentials — use Authorization: Bearer hc_secret_...")
				return
			}

			key, err := svc.ValidateKey(r.Context(), rawKey)
			if err != nil {
				if !errors.Is(err, ErrInvalidKey) {
					// The store never reached a verdict (DB outage, timeout).
					// A 401 here would tell integrators their key went bad
					// mid-outage and start a pointless rotation; a fleet-wide
					// 503 reveals nothing about any individual key, so the
					// ADR-026 anti-enumeration stance holds (#560).
					slog.ErrorContext(r.Context(), "api key validation unavailable",
						"error", err)
					respond.Error(w, r, http.StatusServiceUnavailable, "api_error",
						"authentication_unavailable",
						"authentication is temporarily unavailable — retry shortly")
					return
				}
				// Generic message — never reveal whether a key exists, is
				// expired, or is revoked. Prevents key enumeration. Full
				// reason logged with request-ID. ADR-026.
				slog.WarnContext(r.Context(), "api key validation failed",
					"error", err)
				respond.Unauthorized(w, r, "invalid or expired API key")
				return
			}

			ctx := r.Context()
			ctx = context.WithValue(ctx, tenantIDKey, key.TenantID)
			ctx = context.WithValue(ctx, apiKeyIDKey, key.ID)
			ctx = context.WithValue(ctx, keyTypeKey, KeyType(key.KeyType))
			ctx = postgres.WithLivemode(ctx, key.Livemode)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Require returns middleware that checks if the authenticated key has a specific permission.
// Use this to protect individual route groups.
func Require(perm Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			kt := GetKeyType(r.Context())
			if !HasPermission(kt, perm) {
				respond.Forbidden(w, r,
					"insufficient permissions: this key type does not have "+string(perm)+" access")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireSession gates a subtree to dashboard-session principals only.
// Used for surfaces that are inherently a human act (team membership):
// API keys are machine credentials with no user identity to attribute
// the action to, so they get a 403 with a pointer at the dashboard.
func RequireSession() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if GetKeyType(r.Context()) != KeyTypeSession {
				respond.Forbidden(w, r,
					"this endpoint requires a dashboard session — sign in to the dashboard to manage team membership")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireMethod returns middleware that picks the permission to enforce
// based on HTTP method: GET/HEAD/OPTIONS check `read`, POST/PUT/PATCH/
// DELETE check `write`. Closes the gap where a single Require() in
// front of a chi.Mount applies one permission to a subrouter that
// mixes reads and writes — left unguarded, a key with read access
// could write on every endpoint inside the subtree.
//
// Usage:
//
//	r.With(auth.RequireMethod(auth.PermCustomerRead, auth.PermCustomerWrite)).
//	    Mount("/customers", customerH.Routes())
func RequireMethod(read, write Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			perm := read
			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
				perm = write
			}
			kt := GetKeyType(r.Context())
			if !HasPermission(kt, perm) {
				respond.Forbidden(w, r,
					"insufficient permissions: this key type does not have "+string(perm)+" access")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Context accessors

func TenantID(ctx context.Context) string {
	v, _ := ctx.Value(tenantIDKey).(string)
	return v
}

// WithTenantID returns a derived context carrying tenantID. Middleware uses
// this indirectly via its own WithValue call; background workers (reconciler,
// billing engine charge loop) use this explicitly to seed the tenant ctx
// before calling Stripe-resolver-backed clients so per-tenant credentials
// can be looked up.
func WithTenantID(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantIDKey, tenantID)
}

func KeyID(ctx context.Context) string {
	v, _ := ctx.Value(apiKeyIDKey).(string)
	return v
}

// WithKeyID is the setter counterpart to KeyID. Used by adapters
// outside the auth package (currently `internal/session`) that
// resolve a credential to a key context themselves.
func WithKeyID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, apiKeyIDKey, id)
}

// UserID returns the dashboard user id resolved from the cookie
// session, or "" if the request is on the Bearer (API-key) path.
// Audit log entries use UserID for cookie-authed requests and KeyID
// for Bearer-authed requests as the actor identifier.
func UserID(ctx context.Context) string {
	v, _ := ctx.Value(userIDKey).(string)
	return v
}

// WithUserID is the setter counterpart to UserID. Called by the
// session middleware when a cookie resolves to a user-bound session.
func WithUserID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, userIDKey, id)
}

func GetKeyType(ctx context.Context) KeyType {
	v, _ := ctx.Value(keyTypeKey).(KeyType)
	return v
}

// WithKeyType lets adapters declare the principal type so auth.Require
// keeps working off a single ctx key. Called by `internal/session`
// when a session resolves to its parent key's type.
func WithKeyType(ctx context.Context, kt KeyType) context.Context {
	return context.WithValue(ctx, keyTypeKey, kt)
}

// CustomerActorID returns the customer ID a portal-session middleware
// stamped into ctx, or "" when the request isn't customer-authed.
// audit.Logger consults this to tag rows with actor_type='customer'
// when the request originated from the customer-portal surface, so
// the operator Activity feed renders "by customer" instead of the
// misleading 'system' fallback.
func CustomerActorID(ctx context.Context) string {
	v, _ := ctx.Value(customerActorKey).(string)
	return v
}

// WithCustomerActor stamps a customer ID as the request's actor so downstream
// services (chiefly the audit logger) record who acted.
//
// It is LIVE, and load-bearing. An earlier version of this comment said it was
// "currently unused after the customer-portal removal — retained for a future
// surface", which was false and dangerous in a specific way: it invited a cleanup
// pass to delete the ONE mechanism that attributes an action to a customer rather
// than to "system". It is the whole of ADR-090's customer-actor story — the
// hosted-invoice Pay click (hostedinvoice/handler.go) and the payment-update link
// (payment/public_handler.go) both stamp it, and audit.ResolveActor reads it to
// emit actor_type='customer'.
func WithCustomerActor(ctx context.Context, customerID string) context.Context {
	return context.WithValue(ctx, customerActorKey, customerID)
}

func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return r.Header.Get("X-API-Key")
}

// LivemodeFromRawKey peeks at a raw API key's prefix to recover the mode it
// was minted for, without touching the DB. Returns ok=false for strings that
// don't look like Hardcount API keys so callers can skip the check silently.
func LivemodeFromRawKey(raw string) (live, ok bool) {
	switch {
	case strings.Contains(raw, "_live_"):
		return true, true
	case strings.Contains(raw, "_test_"):
		return false, true
	}
	return false, false
}

// LivemodeFromRequest extracts the mode from whichever auth header the
// request carries (Authorization: Bearer or X-API-Key), if any. Returns
// ok=false when the request has no API key or the key prefix is unrecognised.
func LivemodeFromRequest(r *http.Request) (live, ok bool) {
	raw := extractBearerToken(r)
	if raw == "" {
		return false, false
	}
	return LivemodeFromRawKey(raw)
}
