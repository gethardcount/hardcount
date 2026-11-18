package auth

// Permission is what a route requires of its caller (RequirePermission).
type Permission string

const (
	PermCustomerRead  Permission = "customer:read"
	PermCustomerWrite Permission = "customer:write"

	PermPricingRead  Permission = "pricing:read"
	PermPricingWrite Permission = "pricing:write"

	PermSubscriptionRead  Permission = "subscription:read"
	PermSubscriptionWrite Permission = "subscription:write"

	PermUsageRead   Permission = "usage:read"
	PermUsageWrite  Permission = "usage:write"
	PermUsageIngest Permission = "usage:ingest"

	PermInvoiceRead  Permission = "invoice:read"
	PermInvoiceWrite Permission = "invoice:write"

	PermDunningRead  Permission = "dunning:read"
	PermDunningWrite Permission = "dunning:write"

	// apikey:read and apikey:write gate more than API keys; the route table
	// (internal/api/route_table.go) names each route's permission.
	PermAPIKeyRead  Permission = "apikey:read"
	PermAPIKeyWrite Permission = "apikey:write"

	PermTestClockWrite Permission = "testclock:write"
)

// KeyType is an API key's type, which sets the key's permissions
// (hasPermission).
type KeyType string

const (
	KeyTypeSecret KeyType = "secret" // hc_secret_: every permission
	KeyTypeIngest KeyType = "ingest" // hc_ingest_: PermUsageIngest only (ADR-133)
)

// keyTypes is every KeyType; typePrefixes and checkCreateInput derive from it.
// A new type needs a case in hasPermission and a migration widening the
// api_keys CHECKs.
var keyTypes = []KeyType{KeyTypeSecret, KeyTypeIngest}

// hasPermission reports whether c holds perm. It is the whole permission
// model: a session and a secret key hold every permission, an ingest key holds
// only usage:ingest, and any other caller holds none.
func hasPermission(c Caller, perm Permission) bool {
	switch {
	case c.IsSession():
		// Roles are recorded in user_tenants.role but not enforced (ADR-081).
		return true
	case c.KeyType == KeyTypeSecret:
		return true
	case c.KeyType == KeyTypeIngest:
		// A LiteLLM proxy or another gateway keeps an ingest key in its
		// config, so a leaked copy must not refund, void, read customers or
		// mint keys.
		return perm == PermUsageIngest
	default:
		// No key type (a job's context, or a request nothing authenticated),
		// or an unknown one.
		return false
	}
}
