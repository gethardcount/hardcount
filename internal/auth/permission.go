package auth

import "strings"

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

	// apikey:read and apikey:write gate more than API keys: the access values
	// in internal/api/route_table.go say what each guards.
	PermAPIKeyRead  Permission = "apikey:read"
	PermAPIKeyWrite Permission = "apikey:write"

	PermTestClockWrite Permission = "testclock:write"
)

// Writes reports whether p is a write permission: one that lets its holder
// change an account's data. usage:ingest is not one; it sends usage.
func (p Permission) Writes() bool { return strings.HasSuffix(string(p), ":write") }

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

// hasPermission reports whether c holds perm: a session and a secret key hold
// every permission, an ingest key holds only usage:ingest, and any other
// caller holds none. PermissionRequired decides the session-only and
// any-caller routes. Three handlers refuse more:
//   - the test email refuses any key;
//   - tenantstripe's checkMode refuses the other mode's connection;
//   - the billing run refuses a caller with no tenant.
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
		// No key type, or an unknown one: only a fault could build such a
		// caller.
		return false
	}
}
