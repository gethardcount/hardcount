package activity

import (
	"strconv"
	"strings"
)

// currencySymbols is the dashboard's symbol table (web-v2/src/lib/money.ts,
// CURRENCY_SYMBOLS), so an amount the backend writes into plain text reads
// exactly as the dashboard's own formatter prints it. TestCurrencySymbols
// MatchDashboard keeps the two lists equal.
var currencySymbols = map[string]string{
	"USD": "$", "EUR": "€", "GBP": "£", "JPY": "¥", "CNY": "¥",
	"INR": "₹", "BRL": "R$", "CAD": "CA$", "AUD": "A$", "CHF": "CHF ",
	"SGD": "S$", "HKD": "HK$", "NZD": "NZ$", "SEK": "kr ", "NOK": "kr ",
	"DKK": "kr ", "ZAR": "R", "KRW": "₩", "MXN": "MX$", "PLN": "zł",
	"CZK": "Kč ", "HUF": "Ft ", "ILS": "₪", "AED": "AED ", "SAR": "SAR ",
	"THB": "฿", "MYR": "RM ", "IDR": "Rp ", "PHP": "₱", "VND": "₫",
	"CLP": "CL$", "COP": "CO$", "PEN": "S/", "ARS": "AR$", "TWD": "NT$",
}

// FormatMoney prints an amount the way the dashboard's formatCents does:
// "$1,234.50", "CA$12.00", and the ISO code followed by a space for a
// currency with no symbol. Integer math, hundredths, like every cents field
// in the schema.
func FormatMoney(m Money) string {
	code := strings.ToUpper(strings.TrimSpace(m.Currency))
	symbol, ok := currencySymbols[code]
	if !ok && code != "" {
		symbol = code + " "
	}
	neg := m.Cents < 0
	var abs uint64
	if neg {
		abs = uint64(-(m.Cents + 1)) + 1
	} else {
		abs = uint64(m.Cents)
	}
	whole := strconv.FormatUint(abs/100, 10)
	frac := abs % 100

	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	b.WriteString(symbol)
	for i, r := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	b.WriteByte('.')
	if frac < 10 {
		b.WriteByte('0')
	}
	b.WriteString(strconv.FormatUint(frac, 10))
	return b.String()
}
