package testutil

import "github.com/gethardcount/hardcount/internal/domain"

// BalanceUSD picks the USD row out of a credit GetBalance result, which
// holds one balance per currency (ADR-129). A customer with no USD credit
// reads as a zero USD balance. Wrap the call: BalanceUSD(svc.GetBalance(...)).
func BalanceUSD(balances []domain.CreditBalance, err error) (domain.CreditBalance, error) {
	if err != nil {
		return domain.CreditBalance{}, err
	}
	for _, b := range balances {
		if b.Currency == "USD" {
			return b, nil
		}
	}
	return domain.CreditBalance{Currency: "USD"}, nil
}
