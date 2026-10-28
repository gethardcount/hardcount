package auth

// Permission represents a specific API operation scope.
type Permission string

const (
	PermTenantRead  Permission = "tenant:read"
	PermTenantWrite Permission = "tenant:write"

	PermCustomerRead  Permission = "customer:read"
	PermCustomerWrite Permission = "customer:write"

	PermPricingRead  Permission = "pricing:read"
	PermPricingWrite Permission = "pricing:write"

	PermSubscriptionRead  Permission = "subscription:read"
	PermSubscriptionWrite Permission = "subscription:write"

	PermUsageRead  Permission = "usage:read"
	PermUsageWrite Permission = "usage:write"

	PermInvoiceRead  Permission = "invoice:read"
	PermInvoiceWrite Permission = "invoice:write"

	PermDunningRead  Permission = "dunning:read"
	PermDunningWrite Permission = "dunning:write"

	PermAPIKeyRead  Permission = "apikey:read"
	PermAPIKeyWrite Permission = "apikey:write"

	// PermTestClockWrite grants create / advance / delete on test_clocks.
	// Secret keys and dashboard sessions hold it; platform keys do not.
	PermTestClockWrite Permission = "testclock:write"
)

// KeyType determines the prefix and permission set for an API key.
type KeyType string

const (
	KeyTypePlatform KeyType = "platform" // hc_platform_ — tenant management
	KeyTypeSecret   KeyType = "secret"   // hc_secret_   — full tenant access
	KeyTypeSession  KeyType = "session"  // dashboard cookie session — role-scoped access
)

// TypePrefix returns the type-only prefix (e.g. "hc_secret_"). Used when
// parsing a raw key to identify its type before the mode infix.
func (kt KeyType) TypePrefix() string {
	switch kt {
	case KeyTypePlatform:
		return "hc_platform_"
	default:
		return "hc_secret_"
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

var keyPermissions = map[KeyType]map[Permission]bool{
	KeyTypePlatform: {
		PermTenantRead:  true,
		PermTenantWrite: true,
	},
	KeyTypeSecret: {
		PermCustomerRead:      true,
		PermCustomerWrite:     true,
		PermPricingRead:       true,
		PermPricingWrite:      true,
		PermSubscriptionRead:  true,
		PermSubscriptionWrite: true,
		PermUsageRead:         true,
		PermUsageWrite:        true,
		PermInvoiceRead:       true,
		PermInvoiceWrite:      true,
		PermDunningRead:       true,
		PermDunningWrite:      true,
		PermAPIKeyRead:        true,
		PermAPIKeyWrite:       true,
		PermTestClockWrite:    true,
	},
	// Dashboard sessions inherit the full secret-key permission set today.
	// Invites shipped (ADR-081) but roles are deliberately NOT enforced:
	// user_tenants.role records owner/admin/member for the future split,
	// and every session still gets this full set. When role-scoped
	// permissions land, this map gets replaced by a per-role lookup driven
	// by user_tenants.role.
	KeyTypeSession: {
		PermCustomerRead:      true,
		PermCustomerWrite:     true,
		PermPricingRead:       true,
		PermPricingWrite:      true,
		PermSubscriptionRead:  true,
		PermSubscriptionWrite: true,
		PermUsageRead:         true,
		PermUsageWrite:        true,
		PermInvoiceRead:       true,
		PermInvoiceWrite:      true,
		PermDunningRead:       true,
		PermDunningWrite:      true,
		PermAPIKeyRead:        true,
		PermAPIKeyWrite:       true,
		PermTestClockWrite:    true,
	},
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
