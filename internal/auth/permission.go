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
	// PermUsageIngest sends usage events: POST /v1/usage-events, its
	// /batch, and the LiteLLM callback. It is the only permission an
	// ingest key holds (ADR-133). Backfill and the held-usage release and
	// discard need PermUsageWrite.
	PermUsageIngest Permission = "usage:ingest"

	PermInvoiceRead  Permission = "invoice:read"
	PermInvoiceWrite Permission = "invoice:write"

	PermDunningRead  Permission = "dunning:read"
	PermDunningWrite Permission = "dunning:write"

	PermAPIKeyRead  Permission = "apikey:read"
	PermAPIKeyWrite Permission = "apikey:write"

	// PermTestClockWrite grants create / advance / delete on test_clocks.
	// Secret keys and dashboard sessions hold it; ingest keys do not.
	PermTestClockWrite Permission = "testclock:write"
)

// KeyType is a caller's kind of credential, which sets its permissions: a
// stored key type (secret or ingest), or KeyTypeSession for a dashboard
// session, which no key backs.
type KeyType string

const (
	KeyTypeSecret  KeyType = "secret"  // hc_secret_ — full access to one tenant
	KeyTypeIngest  KeyType = "ingest"  // hc_ingest_ — sends usage events only (ADR-133)
	KeyTypeSession KeyType = "session" // dashboard cookie session, never stored in api_keys
)

// fullAccess is every permission. A secret key and a dashboard session
// hold it.
var fullAccess = map[Permission]bool{
	PermCustomerRead:      true,
	PermCustomerWrite:     true,
	PermPricingRead:       true,
	PermPricingWrite:      true,
	PermSubscriptionRead:  true,
	PermSubscriptionWrite: true,
	PermUsageRead:         true,
	PermUsageWrite:        true,
	PermUsageIngest:       true,
	PermInvoiceRead:       true,
	PermInvoiceWrite:      true,
	PermDunningRead:       true,
	PermDunningWrite:      true,
	PermAPIKeyRead:        true,
	PermAPIKeyWrite:       true,
	PermTestClockWrite:    true,
}

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
	perms, ok := keyPermissions[keyType]
	if !ok {
		return false
	}
	return perms[perm]
}
