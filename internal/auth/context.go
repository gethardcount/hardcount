package auth

import "context"

// contextKey keys the request's context values.
type contextKey string

const (
	callerKey        contextKey = "caller"
	customerActorKey contextKey = "customer_actor_id"
)

// Caller is who a request acts as: a dashboard user's session, or an API key.
type Caller struct {
	TenantID string
	UserID   string  // set for a session
	KeyID    string  // set for a key
	KeyType  KeyType // the key's type; "" for a session
}

// IsSession reports whether c is a dashboard user's session: a user and no
// key.
func (c Caller) IsSession() bool {
	return c.UserID != "" && c.KeyID == ""
}

// Actor names c for a record: "user" and the user's id for a session,
// "api_key" and the key's id for a key, and "", "" for neither. A caller with
// a key is named as the key, as IsSession decides. The names are the audit
// log's actor_type values.
func (c Caller) Actor() (actorType, id string) {
	switch {
	case c.IsSession():
		return "user", c.UserID
	case c.KeyID != "":
		return "api_key", c.KeyID
	}
	return "", ""
}

// WithCaller returns ctx carrying c as the whole caller, in place of any
// caller ctx held. session.Authenticate sets it on every authenticated
// request; no caller means a background job or a route outside authentication.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey, c)
}

// CallerOf returns the caller in ctx, or the zero Caller when there is none.
func CallerOf(ctx context.Context) Caller {
	c, _ := ctx.Value(callerKey).(Caller)
	return c
}

// TenantID returns the caller's tenant in ctx, or "" when there is none.
func TenantID(ctx context.Context) string {
	return CallerOf(ctx).TenantID
}

// WithTenantID returns ctx with tenantID as the caller's tenant; the rest of
// the caller is kept. A background job, or a public route that finds the
// tenant itself, sets it before calling code that needs the tenant, such as
// the per-tenant Stripe client.
func WithTenantID(ctx context.Context, tenantID string) context.Context {
	c := CallerOf(ctx)
	c.TenantID = tenantID
	return WithCaller(ctx, c)
}

// WithUserID returns ctx with id as the caller's user. The account routes
// that session.RequireCaller does not guard (a sign-in, a sign-out, a mode
// switch, an accepted invitation, a password reset) set it, so their audit
// row names the user. The result counts as that user's session.
func WithUserID(ctx context.Context, id string) context.Context {
	c := CallerOf(ctx)
	c.UserID = id
	return WithCaller(ctx, c)
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
