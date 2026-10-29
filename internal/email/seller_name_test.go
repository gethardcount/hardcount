package email

import (
	"context"
	"errors"
	"net/mail"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
)

type failingSettings struct{}

func (failingSettings) Get(context.Context, string) (domain.TenantSettings, error) {
	return domain.TenantSettings{}, errors.New("settings read timed out")
}

// A customer email names the seller only when the tenant has set a company
// name. It used to fill in "Hardcount Billing", in the From name and in the
// header, when the name was unset or the settings read failed, so the
// software vendor read as the biller (sweep 2026-10-21, C8). The
// "Powered by Hardcount Billing" credit in the footer is a credit, not a seller
// name, and stays.
func TestCustomerEmail_NoInventedSellerName(t *testing.T) {
	pdf := []byte("%PDF-1.4 fake\n")
	for _, c := range []struct {
		name     string
		settings SettingsGetter
		wantFrom string
	}{
		{"company name unset", fixedSettings{ts: domain.TenantSettings{}}, ""},
		{"settings read failed", failingSettings{}, ""},
		{"company name set (control)", fixedSettings{ts: domain.TenantSettings{CompanyName: "Acme Billing Co"}}, "Acme Billing Co"},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := newScriptedSMTP(t)
			s := newCCTestSender(t, srv)
			s.SetSettingsGetter(c.settings)
			sends := map[string]func() error{
				"invoice": func() error {
					return s.SendInvoice(context.Background(), "t1", "ap@buyer.test", nil, "Buyer", "INV-1", 1000, "usd", pdf, "", false)
				},
				"credit note": func() error {
					return s.SendCreditNote(context.Background(), "t1", "ap@buyer.test", nil, "Buyer", "CN-1", "INV-1", 1000, "usd", pdf)
				},
			}
			for kind, send := range sends {
				if err := send(); err != nil {
					t.Fatalf("%s: send: %v", kind, err)
				}
				msg, err := mail.ReadMessage(strings.NewReader(srv.lastData()))
				if err != nil {
					t.Fatalf("%s: parse: %v", kind, err)
				}
				from, err := mail.ParseAddress(msg.Header.Get("From"))
				if err != nil {
					t.Fatalf("%s: From %q: %v", kind, msg.Header.Get("From"), err)
				}
				if from.Name != c.wantFrom {
					t.Errorf("%s: From name = %q, want %q", kind, from.Name, c.wantFrom)
				}
				texts := map[string]string{}
				walkParts(t, mail.Header(msg.Header), msg.Body, texts)
				html := strings.ReplaceAll(texts["text/html"], "Powered by Hardcount Billing", "")
				if strings.Contains(html, "Hardcount Billing") {
					t.Errorf("%s: the HTML names Hardcount Billing as the seller", kind)
				}
				if strings.Contains(msg.Header.Get("Subject"), "Hardcount") {
					t.Errorf("%s: subject %q names Hardcount", kind, msg.Header.Get("Subject"))
				}
			}
		})
	}
}
