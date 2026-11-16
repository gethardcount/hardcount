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

// KeyType is a caller's kind of credential, which sets its permissions: a
// stored key type (secret or ingest), or KeyTypeSession for a dashboard
// session, which no key backs.
type KeyType string

const (
	KeyTypeSecret  KeyType = "secret"  // hc_secret_ — full access to one account
	KeyTypeIngest  KeyType = "ingest"  // hc_ingest_ — sends usage events only (ADR-133)
	KeyTypeSession KeyType = "session" // dashboard cookie session, never stored in api_keys
)

// storedKeyTypes are the types a key can be issued and stored as. The type
// prefixes (typePrefixes) and the create check (checkCreateInput) both come
// from this list, and TestStoredKeyTypes_EachHasPrefixesAndPermissions fails
// for a type here with no permission entry. A new type also needs the
// api_keys CHECKs widened by a migration.
var storedKeyTypes = []KeyType{KeyTypeSecret, KeyTypeIngest}

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

// fullAccess is every permission. A secret key and a dashboard session
// hold it.
var fullAccess = func() map[Permission]bool {
	m := make(map[Permission]bool, len(allPermissions))
	for _, p := range allPermissions {
		m[p] = true
	}
	return m
}()

var keyPermissions = map[KeyType]map[Permission]bool{
	KeyTypeSecret: fullAccess,
	// An ingest key sends usage and nothing else (ADR-133). A LiteLLM proxy
	// or another gateway keeps it in its config, so a leaked copy cannot
	// refund, void, read customers or mint keys.
	KeyTypeIngest: {
		PermUsageIngest: true,
	},
	// Every dashboard session gets the full set: roles are recorded in
	// user_tenants.role but not enforced (ADR-081).
	KeyTypeSession: fullAccess,
}

// HasPermission reports whether keyType holds perm. An unknown key type
// holds no permission.
func HasPermission(keyType KeyType, perm Permission) bool {
	return keyPermissions[keyType][perm]
}
