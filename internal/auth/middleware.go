package auth

import (
	"context"
	"net/http"

	"github.com/gethardcount/hardcount/internal/api/respond"
)

type contextKey string

const (
	tenantIDKey      contextKey = "tenant_id"
	apiKeyIDKey      contextKey = "api_key_id"
	userIDKey        contextKey = "user_id"
	keyTypeKey       contextKey = "key_type"
	customerActorKey contextKey = "customer_actor_id"
)

// Require returns middleware that answers 403 unless the caller's kind of
// credential holds perm.
func Require(perm Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if permitted(w, r, perm) {
				next.ServeHTTP(w, r)
			}
		})
	}
}

// permitted reports whether the caller's kind of credential holds perm, and
// answers 403 when it does not.
func permitted(w http.ResponseWriter, r *http.Request, perm Permission) bool {
	if HasPermission(GetKeyType(r.Context()), perm) {
		return true
	}
	respond.Forbidden(w, r, "insufficient permissions: this key type does not have "+string(perm)+" access")
	return false
}

// RequireSession lets only dashboard sessions through. Use it for a human act
// (team membership), where an API key has no user to attribute the action
// to, and for settings both modes share, which a key, made in one mode, must
// not change. Any other caller gets a 403 that ends "sign in to the dashboard
// to <task>".
func RequireSession(task string) func(http.Handler) http.Handler {
	msg := "this endpoint requires a dashboard session — sign in to the dashboard to " + task
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if GetKeyType(r.Context()) != KeyTypeSession {
				respond.Forbidden(w, r, msg)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireMethod returns middleware that picks the permission to enforce
// from the HTTP method. GET, HEAD and OPTIONS check `read`, and POST, PUT,
// PATCH and DELETE check `write`. RequireMethod closes a gap: a single
// Require in front of a chi.Mount applies one permission to a subrouter that
// mixes reads and writes. Without RequireMethod, a key with read access
// could write on every endpoint in the subtree.
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
			if permitted(w, r, perm) {
				next.ServeHTTP(w, r)
			}
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

// WithTenantID returns ctx carrying tenantID. A background job sets it before
// calling code that needs the tenant, such as the per-tenant Stripe client.
func WithTenantID(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantIDKey, tenantID)
}

// KeyID returns the ID of the API key that authenticated the request, or ""
// when no key did.
func KeyID(ctx context.Context) string {
	v, _ := ctx.Value(apiKeyIDKey).(string)
	return v
}

// WithKeyID returns ctx carrying the id of the key that authenticated the
// request.
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

// WithUserID returns ctx carrying the dashboard user of the request's
// session.
func WithUserID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, userIDKey, id)
}

// GetKeyType returns the caller's kind of credential in ctx (KeyType), or ""
// when there is none.
func GetKeyType(ctx context.Context) KeyType {
	v, _ := ctx.Value(keyTypeKey).(KeyType)
	return v
}

// WithKeyType returns ctx carrying the caller's kind of credential, which
// Require, RequireMethod and RequireSession check.
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

// WithCustomerActor returns ctx carrying a customer as the request's actor,
// so the audit row of what a customer did names them (actor_type 'customer')
// rather than 'system' (ADR-090).
func WithCustomerActor(ctx context.Context, customerID string) context.Context {
	return context.WithValue(ctx, customerActorKey, customerID)
}
