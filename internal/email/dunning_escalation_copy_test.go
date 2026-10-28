package email

import (
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
)

// Sweep A2 review (PR #1323): dunning's `pause` final action pauses
// collection on the subscription, which keeps NEW invoices as drafts. A run
// already open on another unpaid invoice of the same subscription keeps
// retrying the customer's card. So the escalation email must not tell the
// customer that automatic collection on their account stopped.
func TestDunningEscalationCopy_PauseSaysNewInvoicesOnly(t *testing.T) {
	for _, o := range []domain.DunningEscalationOutcome{
		{SubscriptionPaused: true},
		{SubscriptionPaused: true, InvoiceWrittenOff: true},
	} {
		sentence, _ := dunningEscalationCopy(o)
		if strings.Contains(sentence, "collection on your account has been paused") {
			t.Errorf("outcome %+v claims all collection stopped:\n%s", o, sentence)
		}
		if !strings.Contains(sentence, "New invoices for your subscription will not be charged automatically.") {
			t.Errorf("outcome %+v does not say only new invoices go uncharged:\n%s", o, sentence)
		}
	}
}
