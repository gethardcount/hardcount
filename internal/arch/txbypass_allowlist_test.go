package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// postgres.TxBypass opens a transaction where Row-Level Security does not
// apply: the query must filter by tenant_id itself. docs/architecture.md
// ("Design rules") lists the kinds of work that may do this, and features,
// comparison, SECURITY, CONTRIBUTING and the RLS-leak runbook link to that
// list. Nothing else checked it, so a new TxBypass on a signed-in path would
// have made every one of those pages wrong with CI green.
//
// This pins every function (outside tests) that names TxBypass, and the kind
// of work it is. A new TxBypass use fails until it is added here with its
// kind; a new kind also needs its words in the architecture.md list.

// bypassKinds maps each kind of work to the words docs/architecture.md uses
// for it. The test fails if those words are missing from the page.
var bypassKinds = map[string]string{
	"background": "background jobs",
	"metrics":    "metrics that count their queues",
	"signin":     "sign-in, sessions, API key lookup and creating an account",
	"public":     "public links",
	"webhook":    "incoming webhooks",
	"stripe":     "Stripe credential settings (`internal/tenantstripe`)",
	"members":    "team members and invites (`internal/dashmembers`)",
	"tool":       "`velox-bench-seed`",
	"definer":    "", // postgres.DB.BeginTx itself
}

// txBypassUsers: "file Func" or "file Recv.Func" -> kind.
var txBypassUsers = map[string]string{
	"cmd/velox-bench-seed/main.go bootstrapFixtures": "tool",

	"internal/platform/postgres/postgres.go DB.BeginTx": "definer",

	// Background jobs: sweeps across tenants, test-clock catch-up (run by
	// the catch-up worker), start-up recovery and backfill.
	"internal/api/middleware/idempotency.go CleanExpired":                                    "background",
	"internal/credit/postgres.go PostgresStore.ListExpiredGrants":                            "background",
	"internal/credit/postgres.go PostgresStore.ListExpiredGrantsForClock":                    "background",
	"internal/creditnote/postgres.go PostgresStore.ListPendingClawbackDrafts":                "background",
	"internal/creditnote/postgres.go PostgresStore.ListPendingClawbackDraftsForClock":        "background",
	"internal/creditnote/postgres.go PostgresStore.ListPendingCreditNoteTaxReversal":         "background",
	"internal/customer/postgres.go PostgresStore.FindByEmailBlindIndex":                      "background", // email suppression check and bounce reports
	"internal/email/outbox.go OutboxStore.claimBatch":                                        "background",
	"internal/email/outbox.go OutboxStore.markCAS":                                           "background",
	"internal/invoice/postgres.go PostgresStore.ListAutoChargePending":                       "background",
	"internal/invoice/postgres.go PostgresStore.ListAutoChargePendingForClock":               "background",
	"internal/invoice/postgres.go PostgresStore.ListFailedWithoutDunningRun":                 "background",
	"internal/invoice/postgres.go PostgresStore.ListPendingTaxCommit":                        "background",
	"internal/invoice/postgres.go PostgresStore.ListPendingTaxRetry":                         "background",
	"internal/invoice/postgres.go PostgresStore.ListPendingTaxRetryForClock":                 "background",
	"internal/invoice/postgres.go PostgresStore.ListPendingTaxReversal":                      "background",
	"internal/invoice/postgres.go PostgresStore.ListStaleChargeHolds":                        "background",
	"internal/invoice/postgres.go PostgresStore.listInflightPayments":                        "background",
	"internal/payment/checkout_sessions_store.go CheckoutSessionStore.ListHostedToReconcile": "background",
	"internal/payment/token.go TokenService.Cleanup":                                         "background",
	"internal/subscription/backfill_intervals.go PostgresStore.intervalBackfillPartitions":   "background",
	"internal/subscription/postgres.go PostgresStore.GetDueBilling":                          "background",
	"internal/subscription/postgres.go PostgresStore.ListExpiredPauseCollections":            "background",
	"internal/subscription/postgres.go PostgresStore.ListExpiredTrials":                      "background",
	"internal/subscription/postgres.go PostgresStore.ListWithThresholds":                     "background",
	"internal/subscription/postgres.go PostgresStore.ListWithThresholdsForClock":             "background",
	"internal/testclock/postgres.go PostgresStore.ListAllAdvancing":                          "background",
	"internal/webhook/outbox.go OutboxStore.ProcessBatch":                                    "background",
	"internal/webhook/outbox.go OutboxStore.attemptRow":                                      "background",
	"internal/webhook/postgres.go PostgresStore.ListPendingDeliveries":                       "background",

	// Queue sizes for metrics and alerts.
	"internal/api/router.go NewServer":                    "metrics",
	"internal/email/outbox.go OutboxStore.FailedCount":    "metrics",
	"internal/email/outbox.go OutboxStore.PendingCount":   "metrics",
	"internal/webhook/outbox.go OutboxStore.FailedCount":  "metrics",
	"internal/webhook/outbox.go OutboxStore.PendingCount": "metrics",

	// Sign-in, sessions, API key lookup, creating an account.
	"internal/auth/postgres.go PostgresStore.GetByPrefix":       "signin",
	"internal/auth/postgres.go PostgresStore.TouchLastUsed":     "signin",
	"internal/session/store.go postgresStore.GetByIDHash":       "signin",
	"internal/session/store.go postgresStore.Insert":            "signin",
	"internal/session/store.go postgresStore.Revoke":            "signin",
	"internal/session/store.go postgresStore.RevokeAllForUser":  "signin",
	"internal/session/store.go postgresStore.UpdateLivemode":    "signin",
	"internal/tenant/bootstrap_run.go RunBootstrap":             "signin",
	"internal/user/postgres.go PostgresStore.AttachTenant":      "signin",
	"internal/user/postgres.go PostgresStore.ConsumeResetToken": "signin",
	"internal/user/postgres.go PostgresStore.Create":            "signin",
	"internal/user/postgres.go PostgresStore.CreateResetToken":  "signin",
	"internal/user/postgres.go PostgresStore.GetByEmail":        "signin",
	"internal/user/postgres.go PostgresStore.GetByID":           "signin",
	"internal/user/postgres.go PostgresStore.LookupResetToken":  "signin",
	"internal/user/postgres.go PostgresStore.SetPassword":       "signin",
	"internal/user/postgres.go PostgresStore.TenantsForUser":    "signin",
	"internal/user/postgres.go PostgresStore.TouchLastLogin":    "signin",
	"internal/user/throttle.go LoginThrottle.Cleanup":           "signin",
	"internal/user/throttle.go LoginThrottle.Reserve":           "signin",
	"internal/user/throttle.go LoginThrottle.proved":            "signin",

	// Public links: hosted invoice, payment-update link, cost page.
	"internal/customer/postgres.go PostgresStore.GetByCostDashboardToken": "public",
	"internal/invoice/postgres.go PostgresStore.GetByPublicToken":         "public",
	"internal/payment/token.go TokenService.Validate":                     "public",

	// Incoming webhooks: Stripe, Postmark.
	"internal/email/outbox.go OutboxStore.GetByID":                                   "webhook",
	"internal/payment/checkout_sessions_store.go CheckoutSessionStore.MarkCompleted": "webhook",

	// Signed-in, one tenant: Stripe credential settings.
	"internal/tenantstripe/postgres.go Store.ClearKeyRejected": "stripe",
	"internal/tenantstripe/postgres.go Store.Delete":           "stripe",
	"internal/tenantstripe/postgres.go Store.GetByID":          "stripe",
	"internal/tenantstripe/postgres.go Store.GetPlaintext":     "stripe",
	"internal/tenantstripe/postgres.go Store.ListByTenant":     "stripe",
	"internal/tenantstripe/postgres.go Store.MarkVerified":     "stripe",
	"internal/tenantstripe/postgres.go Store.MarkVerifyFailed": "stripe",
	"internal/tenantstripe/postgres.go Store.SetWebhookSecret": "stripe",
	"internal/tenantstripe/postgres.go Store.Upsert":           "stripe",

	// Signed-in, one tenant: team members and invites (accepting an invite
	// runs from its emailed link).
	"internal/dashmembers/postgres.go PostgresStore.AcceptInvitation":         "members",
	"internal/dashmembers/postgres.go PostgresStore.CreateInvitation":         "members",
	"internal/dashmembers/postgres.go PostgresStore.GetInvitationByTokenHash": "members",
	"internal/dashmembers/postgres.go PostgresStore.ListInvitations":          "members",
	"internal/dashmembers/postgres.go PostgresStore.ListMembers":              "members",
	"internal/dashmembers/postgres.go PostgresStore.RemoveMember":             "members",
	"internal/dashmembers/postgres.go PostgresStore.RevokeInvitation":         "members",
}

// txBypassFuncs walks internal/ and cmd/ and returns every non-test function
// whose body names TxBypass, as "file Func" or "file Recv.Func".
func txBypassFuncs(t *testing.T, root string) map[string]bool {
	t.Helper()
	found := map[string]bool{}
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil || !namesTxBypass(fd.Body, f.Name.Name == "postgres") {
					continue
				}
				found[rel+" "+funcKey(fd)] = true
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s/: %v", top, err)
		}
	}
	return found
}

func namesTxBypass(body *ast.BlockStmt, inPostgres bool) bool {
	hit := false
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if x.Sel.Name == "TxBypass" {
				hit = true
			}
		case *ast.Ident:
			if inPostgres && x.Name == "TxBypass" {
				hit = true
			}
		}
		return !hit
	})
	return hit
}

func funcKey(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	typ := fd.Recv.List[0].Type
	if s, ok := typ.(*ast.StarExpr); ok {
		typ = s.X
	}
	if ix, ok := typ.(*ast.IndexExpr); ok {
		typ = ix.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}

func TestTxBypass_EveryUseIsListed(t *testing.T) {
	root := repoRoot(t)
	found := txBypassFuncs(t, root)
	if len(found) < 10 {
		t.Fatalf("found %d TxBypass functions — the walk is broken, not the code", len(found))
	}

	var missing, stale []string
	for k := range found {
		if _, ok := txBypassUsers[k]; !ok {
			missing = append(missing, k)
		}
	}
	for k, kind := range txBypassUsers {
		if _, ok := bypassKinds[kind]; !ok {
			t.Errorf("%s: unknown kind %q", k, kind)
		}
		if !found[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	for _, k := range missing {
		t.Errorf("%s uses postgres.TxBypass (no RLS) and is not listed in txBypassUsers. "+
			"Prefer TxTenant. If it must bypass RLS, filter by tenant_id in the query, add it here "+
			"with its kind, and keep the list in docs/architecture.md (Design rules) true.", k)
	}
	for _, k := range stale {
		t.Errorf("%s is listed in txBypassUsers but no longer uses TxBypass: remove it", k)
	}
}

func TestTxBypass_ArchitectureDocNamesEveryKind(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "docs", "architecture.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	used := map[string]bool{}
	for _, kind := range txBypassUsers {
		used[kind] = true
	}
	for kind, words := range bypassKinds {
		if words == "" || !used[kind] {
			continue
		}
		if !strings.Contains(doc, words) {
			t.Errorf("docs/architecture.md must name TxBypass work of kind %q with the words %q", kind, words)
		}
	}
}
