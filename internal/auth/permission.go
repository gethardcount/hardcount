package auth

// Permission is what a route requires of its caller (Require, RequireMethod).
type Permission string

const (
	PermCustomerRead  Permission = "customer:read"
	PermCustomerWrite Permission = "customer:write"

	PermPricingRead  Permission = "pricing:read"
	PermPricingWrite Permission = "pricing:write"

	PermSubscriptionRead  Permission = "subscription:read"
	PermSubscriptionWrite Permission = "subscription:write"

	PermUsageRead  Permission = "usage:read"
	PermUsageWrite Permission = "usage:write"
	// PermUsageIngest sends usage events, one at a time, in a batch or from
	// the LiteLLM callback. A backfill needs PermUsageWrite.
	PermUsageIngest Permission = "usage:ingest"

	PermInvoiceRead  Permission = "invoice:read"
	PermInvoiceWrite Permission = "invoice:write"

	PermDunningRead  Permission = "dunning:read"
	PermDunningWrite Permission = "dunning:write"

	// apikey:read and apikey:write gate more than API keys. apikey:write
	// gates the API key routes (listing keys included), webhook endpoints
	// and the settings routes; apikey:read gates webhook events and their
	// stream, and the audit log and its export.
	PermAPIKeyRead  Permission = "apikey:read"
	PermAPIKeyWrite Permission = "apikey:write"

	// PermTestClockWrite gates the /test-clocks routes.
	PermTestClockWrite Permission = "testclock:write"
)

// KeyType is an API key's type, which sets the key's permissions
// (keyPermissions).
type KeyType string

const (
	KeyTypeSecret KeyType = "secret" // hc_secret_: every permission
	KeyTypeIngest KeyType = "ingest" // hc_ingest_: PermUsageIngest only (ADR-133)
)

// keyTypes is every KeyType; typePrefixes and checkCreateInput derive from it.
// A new type needs a keyPermissions entry and a migration widening the
// api_keys CHECKs.
var keyTypes = []KeyType{KeyTypeSecret, KeyTypeIngest}

// allPermissions is every permission a route can require. A permission left
// out of it is held by no key, so a secret key is refused it (a session still
// holds it).
var allPermissions = []Permission{
	PermCustomerRead, PermCustomerWrite,
	PermPricingRead, PermPricingWrite,
	PermSubscriptionRead, PermSubscriptionWrite,
	PermUsageRead, PermUsageWrite, PermUsageIngest,
	PermInvoiceRead, PermInvoiceWrite,
	PermDunningRead, PermDunningWrite,
	PermAPIKeyRead, PermAPIKeyWrite,
	PermTestClockWrite,
}

// fullAccess is every permission. A secret key holds it.
var fullAccess = func() map[Permission]bool {
	m := make(map[Permission]bool, len(allPermissions))
	for _, p := range allPermissions {
		m[p] = true
	}
	return m
}()

// keyPermissions is what a key of each type may do.
var keyPermissions = map[KeyType]map[Permission]bool{
	KeyTypeSecret: fullAccess,
	// A LiteLLM proxy or another gateway keeps an ingest key in its config,
	// so a leaked copy must not refund, void, read customers or mint keys.
	KeyTypeIngest: {
		PermUsageIngest: true,
	},
}

// hasPermission reports whether c holds perm. A session holds every
// permission: roles are recorded in user_tenants.role but not enforced
// (ADR-081). Any other caller holds what its KeyType holds, and a caller with
// no key type, or an unknown one, holds none.
func hasPermission(c Caller, perm Permission) bool {
	if c.IsSession() {
		return true
	}
	return keyPermissions[c.KeyType][perm]
}
