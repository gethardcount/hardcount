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

// KeyType determines the prefix and permission set for an API key.
type KeyType string

const (
	KeyTypeSecret  KeyType = "secret"  // hc_secret_ — full access to one tenant
	KeyTypeIngest  KeyType = "ingest"  // hc_ingest_ — sends usage events only (ADR-133)
	KeyTypeSession KeyType = "session" // dashboard cookie session, never stored in api_keys
)

// StoredKeyTypes are the key types api_keys holds: the values
// api_keys_key_type_check allows. CreateKey issues only these, and
// ValidateKey reads only their prefixes.
var StoredKeyTypes = []KeyType{KeyTypeSecret, KeyTypeIngest}

// TypePrefix returns the type-only prefix (e.g. "hc_secret_"). Used when
// parsing a raw key to identify its type before the mode infix. A type
// that is never stored (a session) has no prefix, so it returns "".
func (kt KeyType) TypePrefix() string {
	switch kt {
	case KeyTypeSecret:
		return "hc_secret_"
	case KeyTypeIngest:
		return "hc_ingest_"
	default:
		return ""
	}
}

// KeyPrefix returns the full "hc_{type}_{mode}_" prefix used on new keys.
// Stripe-style: hc_secret_live_..., hc_secret_test_..., etc. A visible mode
// infix lets operators spot "test key in prod config" misrouting without
// decoding the key.
func KeyPrefix(kt KeyType, livemode bool) string {
	mode := "test"
	if livemode {
		mode = "live"
	}
	return kt.TypePrefix() + mode + "_"
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
	// Dashboard sessions inherit the full secret-key permission set today.
	// Invites shipped (ADR-081) but roles are deliberately NOT enforced:
	// user_tenants.role records owner/admin/member for the future split,
	// and every session still gets this full set. When role-scoped
	// permissions land, this map gets replaced by a per-role lookup driven
	// by user_tenants.role.
	KeyTypeSession: fullAccess,
}

func HasPermission(keyType KeyType, perm Permission) bool {
	perms, ok := keyPermissions[keyType]
	if !ok {
		return false
	}
	return perms[perm]
}

func AllPermissions(keyType KeyType) []Permission {
	perms, ok := keyPermissions[keyType]
	if !ok {
		return nil
	}
	result := make([]Permission, 0, len(perms))
	for p := range perms {
		result = append(result, p)
	}
	return result
}
