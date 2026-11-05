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

// An email reaches email_outbox in one of two ways. An OutboxSender method
// built on enqueueTx inserts the row on the caller's transaction, so the
// email commits with its change: the money emails do this (ADR-040). A
// method built on enqueue inserts it in a transaction of its own, after the
// caller's change committed, so a crash in between loses the email. That is
// right only for a send that has no business transaction to join.
//
// This pins every production call of a standalone sender, with the reason it
// has no business transaction. A new call fails until it is listed here. The
// pin is by function and sender: a second call of the same sender in a listed
// function is covered by that entry's reason.

// standaloneEmailSends maps each production call of a standalone OutboxSender
// method, as "<file> <Func> <Sender>" or "<file> <Recv.Func> <Sender>", to why
// the send has no business transaction to ride.
var standaloneEmailSends = map[string]string{
	"internal/invoice/invoice_email.go Handler.emailInvoice SendInvoice": "The operator's Email invoice has no business transaction: the email is the action, and the request returns the enqueue error. " +
		"The collector's automatic send stamps invoice_emailed_at only after the enqueue, so its next visit sends again.",
	"internal/api/adapters.go noPaymentMethodNotifierAdapter.NotifyNoPaymentMethod SendPaymentSetupRequest": "The engine stamps no_pm_notified_at only after the send, so its next visit sends again. " +
		"The operator's resend has no business transaction and returns the error.",
	"internal/paymentmethods/handler.go Handler.operatorSendSetupEmail SendPaymentSetupLink": "An operator action with no business transaction (a Stripe setup session, then the email). The error goes back to the operator.",
	"internal/creditnote/handler.go Handler.sendEmail SendCreditNote":                        "The operator's Email credit note: the note is already issued, and the email is the whole action. The error goes back to the operator.",
	"internal/user/handler.go Handler.requestPasswordReset SendPasswordReset":                "Account plane, no money. A single-use reset token is useless without the email, and the user asks again.",
	"internal/api/adapters.go passwordResetEmailAdapter.SendPasswordReset SendPasswordReset": "Forwards the password reset to OutboxSender, for user.Handler.requestPasswordReset.",
	"internal/dashmembers/service.go Service.Invite SendMemberInvite": "Account plane. The invitation is written on a TxBypass transaction, which sets no app.livemode, so the email_outbox insert would be refused on it. " +
		"An invite whose email is lost shows as pending on the Team page, and the operator can revoke and send it again.",
}

// standaloneEmailSenders returns the OutboxSender methods whose body calls
// s.enqueue, the post-commit path.
func standaloneEmailSenders(t *testing.T, root string) map[string]bool {
	t.Helper()
	path := filepath.Join(root, "internal", "email", "outbox_sender.go")
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil || funcKey(fd) != "OutboxSender."+fd.Name.Name || len(fd.Recv.List[0].Names) == 0 {
			continue
		}
		recv := fd.Recv.List[0].Names[0].Name
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if ok && sel.Sel.Name == "enqueue" {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == recv {
					names[fd.Name.Name] = true
				}
			}
			return true
		})
	}
	return names
}

func TestStandaloneEmailSendsAreListed(t *testing.T) {
	root := repoRoot(t)
	senders := standaloneEmailSenders(t, root)
	if len(senders) < 6 {
		t.Fatalf("found %d OutboxSender methods that call s.enqueue; want at least 6 (SendInvoice, SendCreditNote, …). The parse is broken, not the code", len(senders))
	}

	found := map[string]bool{} // "<file> <Func> <Sender>" of each call
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			// The email package owns the senders, and its drain hands
			// committed rows to SMTP.
			if d.IsDir() && rel == "internal/email" {
				return filepath.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && senders[sel.Sel.Name] {
						found[rel+" "+funcKey(fd)+" "+sel.Sel.Name] = true
					}
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s/: %v", top, err)
		}
	}

	var problems []string
	for key := range found {
		if _, ok := standaloneEmailSends[key]; !ok {
			i := strings.LastIndex(key, " ")
			problems = append(problems, key[:i]+" calls "+key[i+1:]+", which queues the email after the caller's transaction commits, so a crash in between loses it. "+
				"Send it with the OutboxSender ...Tx method on the business transaction (ADR-040). "+
				"If the send has no business transaction, add it to standaloneEmailSends with the reason")
		}
	}
	for key, reason := range standaloneEmailSends {
		if !found[key] {
			problems = append(problems, "standaloneEmailSends lists "+key+", but that function no longer calls that sender: remove it")
		}
		if strings.TrimSpace(reason) == "" {
			problems = append(problems, "standaloneEmailSends lists "+key+" with no reason: say why the send has no business transaction")
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
	t.Logf("%d standalone email senders, called from %d pairs of production function and sender", len(senders), len(found))
}
