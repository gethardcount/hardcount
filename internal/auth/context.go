package auth

import "context"

// contextKey keys the request's context values.
type contextKey string

const (
	callerKey        contextKey = "caller"
	customerActorKey contextKey = "customer_actor_id"
)

// Caller is who a request acts as: a dashboard user's session, or an API key.
// Never both a UserID and a KeyID: session.Authenticate sets one or the other,
// and WithUserID runs only on account routes, which carry no caller. A
// background job, a webhook or a public page carries only a tenant
// (WithTenantID), or no caller.
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

// Actor names c in the audit log's actor_type words: "user" and the user's
// id for a session, "api_key" and the key's id for any caller with a key,
// and "", "" for neither. audit.ResolveActor says what a new name needs.
func (c Caller) Actor() (actorType, id string) {
	switch {
	case c.IsSession():
		return "user", c.UserID
	case c.KeyID != "":
		return "api_key", c.KeyID
	}
	return "", ""
}

// WithCaller returns ctx with c as its whole caller, replacing any caller ctx
// held. session.Authenticate sets it for each authenticated request.
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
// outside authentication (route_table.go's outsideAuth entries) set it, so
// their audit row names the user. Each calls it on a context with no caller, so the
// result is that user's session (IsSession).
func WithUserID(ctx context.Context, id string) context.Context {
	c := CallerOf(ctx)
	c.UserID = id
	return WithCaller(ctx, c)
}

// CustomerActorID returns the customer WithCustomerActor put on ctx, or ""
// when none.
func CustomerActorID(ctx context.Context) string {
	v, _ := ctx.Value(customerActorKey).(string)
	return v
}

// WithCustomerActor returns ctx with customerID as the actor, for work a
// customer did: on a public page, or in the Stripe event such a page caused.
// audit.ResolveActor then names the customer (actor_type 'customer') ahead
// of any caller.
func WithCustomerActor(ctx context.Context, customerID string) context.Context {
	return context.WithValue(ctx, customerActorKey, customerID)
}
