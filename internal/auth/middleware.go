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

// Middleware validates the request's API key and puts its tenant, key id,
// key type and mode in the context.
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
					// The store reached no verdict (a database outage or a
					// timeout). A 401 here would tell integrators their key
					// went bad mid-outage, and start a pointless rotation. A
					// 503 for every caller reveals nothing about any one
					// key, so the ADR-026 rule against key enumeration
					// holds (#560).
					slog.ErrorContext(r.Context(), "api key validation unavailable",
						"error", err)
					respond.Error(w, r, http.StatusServiceUnavailable, "api_error",
						"authentication_unavailable",
						"authentication is temporarily unavailable — retry shortly")
					return
				}
				// Use a generic message that never says whether a key
				// exists, has expired or was revoked, so keys cannot be
				// enumerated (ADR-026). The full reason is logged with the
				// request ID.
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

// Require returns middleware that checks that the authenticated key has a
// specific permission. Use it to protect individual route groups.
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
// from the HTTP method. GET, HEAD and OPTIONS check `read`, and POST, PUT,
// PATCH and DELETE check `write`. A single Require in front of a chi.Mount
// applies one permission to a subrouter that mixes reads and writes. Left
// unguarded, a key with read access could then write on every endpoint in
// the subtree.
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

// TenantID returns the authenticated tenant ID in ctx, or "" when there is
// none.
func TenantID(ctx context.Context) string {
	v, _ := ctx.Value(tenantIDKey).(string)
	return v
}

// WithTenantID returns a derived context carrying tenantID. Middleware sets
// the same key with its own WithValue call. Background workers (the
// reconciler, the billing engine's charge loop) call WithTenantID to set the
// tenant before they call clients backed by the Stripe resolver, so the
// tenant's own credentials can be looked up.
func WithTenantID(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantIDKey, tenantID)
}

// KeyID returns the ID of the API key that authenticated the request, or ""
// when no key did.
func KeyID(ctx context.Context) string {
	v, _ := ctx.Value(apiKeyIDKey).(string)
	return v
}

// WithKeyID is the setter counterpart to KeyID. Adapters outside the auth
// package that resolve a credential to a key context themselves call it
// (currently `internal/session`).
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

// GetKeyType returns the principal's key type in ctx, or "" when there is
// none.
func GetKeyType(ctx context.Context) KeyType {
	v, _ := ctx.Value(keyTypeKey).(KeyType)
	return v
}

// WithKeyType lets adapters declare the principal type, so auth.Require
// keeps working from a single ctx key. `internal/session` calls it when a
// session resolves to its parent key's type.
func WithKeyType(ctx context.Context, kt KeyType) context.Context {
	return context.WithValue(ctx, keyTypeKey, kt)
}

// CustomerActorID returns the customer ID that a portal-session middleware
// stamped into ctx, or "" when the request is not customer-authenticated.
// audit.Logger reads it to tag rows with actor_type='customer' when the
// request came from the customer-portal surface. The operator Activity feed
// then shows "by customer" instead of the misleading 'system' fallback.
func CustomerActorID(ctx context.Context) string {
	v, _ := ctx.Value(customerActorKey).(string)
	return v
}

// WithCustomerActor stamps a customer ID as the request's actor, so
// downstream services (chiefly the audit logger) record who acted.
//
// It is in use and needed, so do not remove it as unused. It is the one
// mechanism that attributes an action to a customer rather than to
// "system", and the whole of ADR-090's customer-actor design. The
// hosted-invoice Pay click (hostedinvoice/handler.go) and the payment-update
// link (payment/public_handler.go) both stamp it. audit.ResolveActor reads
// it to write actor_type='customer'.
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
