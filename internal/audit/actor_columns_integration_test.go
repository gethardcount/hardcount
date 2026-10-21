package audit_test

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestResolveActorKinds_MatchEveryActorCheck keeps ResolveActor's kinds and
// the CHECK of every column it fills equal (the enum and CHECK sync rule):
// the audit log, and since ADR-128 PR 6 the email outbox and the charge
// attempts. A kind a CHECK did not list would fail that insert: an email
// never queued, an attempt row lost.
//
// The kinds come from the four contexts below, one per branch of
// ResolveActor, written by hand. This test does not see a kind added to
// ResolveActor without a context here: whoever adds a branch adds its
// context to this list, and the CHECKs then fail until they allow it.
func TestResolveActorKinds_MatchEveryActorCheck(t *testing.T) {
	testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)

	base := context.Background()
	kinds := map[string]bool{}
	for _, ctx := range []context.Context{
		base,
		auth.WithUserID(base, "u"),
		auth.WithKeyID(base, "k"),
		auth.WithCustomerActor(base, "c"),
	} {
		kind, _ := audit.ResolveActor(ctx)
		kinds[kind] = true
	}
	want := sortedKeys(kinds)

	for _, conname := range []string{
		"audit_log_actor_type_check",
		"email_outbox_actor_type_check",
		"invoice_charge_attempts_actor_type_check",
	} {
		var def string
		if err := admin.QueryRowContext(base,
			`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1`, conname,
		).Scan(&def); err != nil {
			t.Errorf("read %s: %v", conname, err)
			continue
		}
		inCheck := map[string]bool{}
		for _, m := range regexp.MustCompile(`'(\w+)'`).FindAllStringSubmatch(def, -1) {
			inCheck[m[1]] = true
		}
		if got := sortedKeys(inCheck); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s allows %v; ResolveActor returns %v", conname, got, want)
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
