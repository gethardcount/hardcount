package auth

// Permission represents a specific API operation scope.
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
	// PermUsageIngest sends usage events. It is the only permission an
	// ingest key holds.
	PermUsageIngest Permission = "usage:ingest"

	PermInvoiceRead  Permission = "invoice:read"
	PermInvoiceWrite Permission = "invoice:write"

	PermDunningRead  Permission = "dunning:read"
	PermDunningWrite Permission = "dunning:write"

	// apikey:read and apikey:write gate more than API keys. apikey:write
	// gates the API key routes (listing keys included), webhook endpoints
	// and settings; apikey:read gates webhook events, their stream and the
	// audit log (router.go).
	PermAPIKeyRead  Permission = "apikey:read"
	PermAPIKeyWrite Permission = "apikey:write"

	// PermTestClockWrite gates the /test-clocks routes.
	PermTestClockWrite Permission = "testclock:write"
)

// KeyType is an API key's type, which sets the key's permissions
// (keyPermissions).
type KeyType string

const (
	KeyTypeSecret KeyType = "secret" // hc_secret_ — full access to one account
	KeyTypeIngest KeyType = "ingest" // hc_ingest_ — sends usage events only (ADR-133)
)

// keyTypes is every KeyType. The type prefixes (typePrefixes) and the create
// check (checkCreateInput) both come from this list. A new type needs a
// permission entry (TestKeyTypes_EachHasPermissions) and a migration that
// widens the api_keys CHECKs (TestKeyShape_ParserAndTableAgree).
var keyTypes = []KeyType{KeyTypeSecret, KeyTypeIngest}

// allPermissions is every permission a route can require.
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
	// An ingest key sends usage and nothing else (ADR-133). A LiteLLM proxy
	// or another gateway keeps it in its config, so a leaked copy cannot
	// refund, void, read customers or mint keys.
	KeyTypeIngest: {
		PermUsageIngest: true,
	},
}

// HasPermission reports whether c holds perm. A session holds every
// permission: roles are recorded in user_tenants.role but not enforced
// (ADR-081). A key holds its type's permissions, whoever else is named on
// the caller. A caller that is neither, or a key of an unknown type, holds
// none.
func HasPermission(c Caller, perm Permission) bool {
	if c.IsSession() {
		return true
	}
	return keyPermissions[c.KeyType][perm]
}
