package activity

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

func TestFormatMoney(t *testing.T) {
	for _, tc := range []struct {
		cents    int64
		currency string
		want     string
	}{
		{123450, "USD", "$1,234.50"},
		{1200, "CAD", "CA$12.00"},
		{1200, "cad", "CA$12.00"},
		{5, "EUR", "€0.05"},
		{123456789, "INR", "₹1,234,567.89"},
		{-1500, "GBP", "-£15.00"},
		{1000, "XYZ", "XYZ 10.00"},
		{1000, "CHF", "CHF 10.00"},
		{0, "USD", "$0.00"},
	} {
		if got := FormatMoney(Money{Cents: tc.cents, Currency: tc.currency}); got != tc.want {
			t.Errorf("FormatMoney(%d, %q) = %q, want %q", tc.cents, tc.currency, got, tc.want)
		}
	}
}

// TestCurrencySymbolsMatchDashboard keeps the backend's plain-text money
// equal to the dashboard's formatter: the same symbol for every currency
// web-v2/src/lib/money.ts knows, and no currency only one side knows.
func TestCurrencySymbolsMatchDashboard(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "web-v2", "src", "lib", "money.ts"))
	if err != nil {
		t.Fatalf("read the dashboard's money formatter: %v", err)
	}
	block := regexp.MustCompile(`(?s)CURRENCY_SYMBOLS: Record<string, string> = \{(.*?)\n\}`).FindSubmatch(src)
	if block == nil {
		t.Fatal("CURRENCY_SYMBOLS not found in web-v2/src/lib/money.ts")
	}
	dash := map[string]string{}
	for _, m := range regexp.MustCompile(`([A-Z]{3}): '([^']*)'`).FindAllSubmatch(block[1], -1) {
		dash[string(m[1])] = string(m[2])
	}
	if len(dash) != len(currencySymbols) {
		t.Errorf("the dashboard knows %d currencies, the backend %d", len(dash), len(currencySymbols))
	}
	for code, sym := range dash {
		if currencySymbols[code] != sym {
			t.Errorf("%s: dashboard %q, backend %q", code, sym, currencySymbols[code])
		}
	}
}

func TestDeclineReason(t *testing.T) {
	for _, tc := range []struct{ code, provider, want string }{
		{"insufficient_funds", "Your card has insufficient funds.", "Card declined: insufficient funds"},
		{"INSUFFICIENT_FUNDS", "", "Card declined: insufficient funds"},
		{"expired_card", "", "Card declined: expired card"},
		{"incorrect_cvc", "", "Card declined: incorrect security code"},
		{"lost_card", "", "Card declined: reported lost or stolen"},
		{"stolen_card", "", "Card declined: reported lost or stolen"},
		{"generic_decline", "", "Card declined by the bank"},
		// Unmapped: the provider's own text, without Velox's wrapper.
		{"pickup_card", "payment failed: Your card cannot be used.", "Your card cannot be used."},
		{"", "payment state unknown: Stripe timed out.", "Stripe timed out."},
		{"", "", ""},
	} {
		if got := DeclineReason(tc.code, tc.provider); got != tc.want {
			t.Errorf("DeclineReason(%q, %q) = %q, want %q", tc.code, tc.provider, got, tc.want)
		}
	}
}

func TestStampKeepsRecordedOnlyForPinnedEntities(t *testing.T) {
	occurred := time.Date(2027, 2, 1, 4, 30, 0, 0, time.UTC)
	recorded := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	l := Stamp(Line{Title: "Invoice finalized"}, occurred, recorded, true)
	if !l.OccurredAt.Equal(occurred) || l.RecordedAt == nil || !l.RecordedAt.Equal(recorded) {
		t.Errorf("pinned: occurred %v recorded %v", l.OccurredAt, l.RecordedAt)
	}
	if l.Detail == nil {
		t.Error("Stamp leaves detail nil; it must serialize as a list")
	}
	if l := Stamp(Line{}, occurred, recorded, false); l.RecordedAt != nil {
		t.Errorf("real clock: recorded %v, want none", l.RecordedAt)
	}
	if l := Stamp(Line{}, occurred, time.Time{}, true); l.RecordedAt != nil {
		t.Errorf("pinned without a real stamp (legacy row): recorded %v, want none", l.RecordedAt)
	}
}

func TestActorWords(t *testing.T) {
	for _, tc := range []struct {
		kind, name, want string
	}{
		{"user", "alice@example.com", "by alice@example.com"},
		{"api_key", "billing-sync", "by API key billing-sync"},
		{"customer", "Acme", "Customer"},
		{"system", "", "Automatic"},
		{"nonsense", "x", ""},
	} {
		if got := ActorText(ActorFromAudit(tc.kind, tc.name)); got != tc.want {
			t.Errorf("%s/%s: %q, want %q", tc.kind, tc.name, got, tc.want)
		}
	}
	if got := ActorText(&Actor{Kind: ActorCustomer, Via: ViaPaymentPage}); got != "Customer, on the payment page" {
		t.Errorf("customer on the payment page: %q", got)
	}
	if got := ActorText(&Actor{Kind: ActorCustomer, Via: ViaPortal}); got != "Customer, in the portal" {
		t.Errorf("customer in the portal: %q", got)
	}
}

func TestMetaText(t *testing.T) {
	due := time.Date(2027, 2, 15, 4, 30, 0, 0, time.UTC)
	l := Line{
		Detail: []Segment{
			{L(LinkCreditNote, "vlx_cn_1", "CN-000012")},
			{M(1500, "USD"), T(" refunded to card")},
			{T("Respond by "), At(due)},
			{},
		},
		Actor: Automatic(),
	}
	if got, want := MetaText(l), "CN-000012 · $15.00 refunded to card · Respond by Feb 15, 2027 04:30 UTC · Automatic"; got != want {
		t.Errorf("MetaText = %q, want %q", got, want)
	}
}

func TestEmailOutcomeWords(t *testing.T) {
	for _, tc := range []struct {
		status, delivery, failure string
		word                      string
		tone                      Tone
	}{
		{"pending", "unknown", "", "queued", ToneNeutral},
		{"dispatched", "unknown", "", "sent", ToneNeutral},
		{"dispatched", "delivered", "", "delivered", ToneNeutral},
		{"dispatched", "bounced", "", "bounced", ToneFailure},
		{"dispatched", "complained", "", "marked as spam", ToneFailure},
		{"failed", "unknown", SendFailureSMTPNotConfigured, "couldn't be sent", ToneFailure},
		{"skipped", "unknown", "", "not sent", ToneNeutral},
	} {
		out := Outcome(tc.status, tc.delivery, tc.failure)
		if out.Word != tc.word || out.Tone != tc.tone {
			t.Errorf("%s/%s: %q %s, want %q %s", tc.status, tc.delivery, out.Word, out.Tone, tc.word, tc.tone)
		}
	}
	// "Failed payment notice failed" read as two events; the name is a noun
	// and the outcome never repeats "failed".
	if got := EmailClause("payment_failed", "failed", "", ""); got != "Failed payment notice couldn't be sent" {
		t.Errorf("clause %q", got)
	}
}
