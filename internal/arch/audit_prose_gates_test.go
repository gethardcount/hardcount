package arch

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// sourceFiles walks internal/ and hands back every non-test .go file.
func sourceFiles(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	root := internalDir(t)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(filepath.Dir(root), path)
		out[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("walked 0 source files — the gate is broken, not the code")
	}
	return out
}

// markSkipCallers is the CANONICAL list of paths that declare "this 2xx mutated
// nothing" (audit.MarkSkip). It is duplicated in prose in two places that keep
// going stale — internal/audit/context.go's "Live callers" list and ADR-090 §4 —
// so it is pinned HERE, against the code, and those two now point at this test
// rather than trying to enumerate from memory.
//
// Adding a MarkSkip call is a real decision: it tells the coverage detector that a
// successful mutating request legitimately wrote no audit row. If you add one,
// add it here and say why. If this list is longer than you expected, that is the
// point — a silent MarkSkip is a route quietly opting out of audit coverage.
// markSkipCaller is a declared opt-out of audit coverage: n calls in one file,
// and the reason they are legitimate.
//
// The COUNT is load-bearing. The first version of this gate keyed on the FILE
// alone — so adding a tenth MarkSkip to a file that already declared nine passed
// CI silently, while this very comment claimed "add a MarkSkip call without
// declaring it and CI fails". A gate that under-enforces its own stated contract
// is the failure class this whole arc is about, reproduced inside the fix for it.
// Pinning n means a NEW opt-out in an existing file breaks the build.
type markSkipCaller struct {
	n   int
	why string
}

var markSkipCallers = map[string]markSkipCaller{
	"internal/billing/create_preview_handler.go": {1, "invoice create_preview — read-only dry run"},
	"internal/recipe/handler.go":                 {1, "recipe preview — read-only render"},
	"internal/recipe/service.go":                 {1, "recipe re-apply that installs nothing"},
	"internal/tenant/settings.go":                {1, "settings save that changed no field"},
	"internal/usage/handler.go":                  {1, "duplicate usage ingest — idempotency key already exists, no event row written"},
	"internal/usage/held_handler.go":             {2, "held-usage release or discard that found no held row to change (nothing held, every meter still missing, or every row after the customer's current time)"},
	"internal/user/service.go":                   {1, "a mode switch to the mode the session is already in"},
	"internal/user/handler.go":                   {3, "logout with NO cookie; logout with a stale cookie (both revoke nothing); a password reset that issued no token — unknown address OR an account over its send cap, which share ONE fixed-200 path (that fixedness is the enumeration defence)"},
	"internal/api/adapters.go":                   {2, "hosted-invoice payment-session reuse — a second Pay click returns the same Stripe session, no new claim row"},
	"internal/api/middleware/idempotency.go":     {1, "idempotency replay — the cached response of a request whose original DID emit"},
	"internal/creditnote/handler.go":             {1, "credit-note issue that defers"},
	"internal/subscription/handler.go":           {3, "resume collection on a subscription that is not paused; clear a scheduled cancel when none is scheduled; cancel a plan change when none is scheduled (all change nothing)"},
}

// TestMarkSkipCallers_MatchTheCode is the gate on the class that drifted in all
// three audit rounds: a hand-maintained list of "who calls MarkSkip" that nobody
// re-derives when a caller is added. It also holds that only the audit package
// calls request.MarkAudited (the mark MarkSkip and an emitted row set) and
// request.ClientIPIfAny, the two calls that accept a missing record.
func TestMarkSkipCallers_MatchTheCode(t *testing.T) {
	call := regexp.MustCompile(`(?:audit\.)?MarkSkip\(`)
	auditOnly := regexp.MustCompile(`request\.(?:MarkAudited|ClientIPIfAny)\(`)

	found := map[string]int{}
	var outsideAudit []string
	for path, src := range sourceFiles(t) {
		if !strings.HasPrefix(path, "internal/audit/") {
			for _, m := range auditOnly.FindAllString(src, -1) {
				outsideAudit = append(outsideAudit, path+": "+m)
			}
		}
		// The definition itself, and the package's own doc, are not callers.
		if path == "internal/audit/context.go" {
			continue
		}
		if n := len(call.FindAllString(src, -1)); n > 0 {
			found[path] = n
		}
	}

	var undeclared, stale, miscounted []string
	for path, n := range found {
		d, ok := markSkipCallers[path]
		if !ok {
			undeclared = append(undeclared, fmt.Sprintf("%s (%d call(s))", path, n))
			continue
		}
		if d.n != n {
			miscounted = append(miscounted, fmt.Sprintf("%s: %d call(s) in the code, %d declared — %s", path, n, d.n, d.why))
		}
	}
	for path := range markSkipCallers {
		if found[path] == 0 {
			stale = append(stale, path)
		}
	}
	sort.Strings(undeclared)
	sort.Strings(stale)
	sort.Strings(miscounted)
	sort.Strings(outsideAudit)

	if len(outsideAudit) > 0 {
		t.Errorf(`only the audit package may call request.MarkAudited and request.ClientIPIfAny:

  %s

MarkAudited claims audit coverage with no row behind it, and ClientIPIfAny answers ""
where a request's record is missing. Outside the audit package, call audit.MarkSkip
(declared in markSkipCallers) or read request.Of(ctx).ClientIP().`, strings.Join(outsideAudit, "\n  "))
	}

	if len(undeclared) > 0 {
		t.Errorf(`%d file(s) call audit.MarkSkip but are NOT declared in markSkipCallers:

  %s

MarkSkip tells the coverage detector that a successful MUTATING request legitimately
wrote no audit row. That is a route opting out of audit coverage, and it must be a
decision someone made on purpose — not a line that appeared. Add the file to
markSkipCallers with its call COUNT and a one-line reason.`,
			len(undeclared), strings.Join(undeclared, "\n  "))
	}
	if len(miscounted) > 0 {
		t.Errorf(`%d file(s) have a DIFFERENT number of MarkSkip calls than declared:

  %s

A NEW MarkSkip in a file that already had one is a NEW route opting out of audit
coverage — the count is what makes that visible. If the new call is intended, bump
the count and extend the reason to cover it.`, len(miscounted), strings.Join(miscounted, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf(`%d entr(y/ies) in markSkipCallers call MarkSkip nowhere:

  %s

The caller was removed but the list was not. A list that names paths which no longer
exist is exactly the kind of confident-and-wrong documentation this gate exists to
prevent.`, len(stale), strings.Join(stale, "\n  "))
	}
}
