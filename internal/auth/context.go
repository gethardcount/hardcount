package auth

import "context"

// The caller values a request's context carries: who authenticated it
// (session.Authenticate sets the tenant, the session's user or the key, and
// the kind of credential) and, on a public customer page, the customer who
// acted.

type contextKey string

const (
	tenantIDKey      contextKey = "tenant_id"
	apiKeyIDKey      contextKey = "api_key_id"
	userIDKey        contextKey = "user_id"
	keyTypeKey       contextKey = "key_type"
	customerActorKey contextKey = "customer_actor_id"
)

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

// UserID returns the dashboard user of the request's session, or "" when a
// key authenticated it. The audit log names the actor by UserID for a session
// and by KeyID for a key.
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

// CustomerActorID returns the customer a public customer page set as the
// request's actor (WithCustomerActor), or "" when no customer acted.
// audit.Logger reads it to write actor_type 'customer'.
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
