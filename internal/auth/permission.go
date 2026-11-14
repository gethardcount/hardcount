package auth

import "strings"

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

// KeyType determines the prefix and permission set for an API key.
type KeyType string

const (
	KeyTypeSecret  KeyType = "secret"  // hc_secret_ — full access to one tenant
	KeyTypeIngest  KeyType = "ingest"  // hc_ingest_ — sends usage events only (ADR-133)
	KeyTypeSession KeyType = "session" // dashboard cookie session, never stored in api_keys
)

// keyPrefix returns a new key's prefix, "hc_{type}_{mode}_": hc_secret_live_,
// hc_ingest_test_ and so on. The visible mode lets an operator spot a test
// key in a production config. kt is secret or ingest, the types api_keys
// holds.
func keyPrefix(kt KeyType, livemode bool) string {
	mode := "test"
	if livemode {
		mode = "live"
	}
	return "hc_" + string(kt) + "_" + mode + "_"
}

// keyPrefixes are the four prefixes a key can have: its type, then its mode.
// api_keys_prefix_shape_check states the same shape for the stored prefix.
var keyPrefixes = [...]string{"hc_secret_live_", "hc_secret_test_", "hc_ingest_live_", "hc_ingest_test_"}

// lookupPrefix returns the stored lookup prefix of raw, its type-and-mode
// prefix and the first keyPrefixLen hex characters of its secret, or false
// when raw is not a key: one of keyPrefixes, then exactly 64 lowercase hex
// characters.
func lookupPrefix(raw string) (string, bool) {
	for _, p := range keyPrefixes {
		secret, ok := strings.CutPrefix(raw, p)
		if !ok {
			continue
		}
		if len(secret) != 2*keySecretLen || strings.Trim(secret, "0123456789abcdef") != "" {
			return "", false
		}
		return p + secret[:keyPrefixLen], true
	}
	return "", false
}

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
	// Dashboard sessions get the full secret-key permission set. Roles are
	// recorded but, on purpose, not enforced (ADR-081). user_tenants.role
	// records owner, admin or member for a later split, and every session
	// still gets this full set. When role-scoped permissions are built, a
	// per-role lookup on user_tenants.role replaces this map.
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
