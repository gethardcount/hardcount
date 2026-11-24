// The Credit page's balance filter, read from the URL (?balance=out|low):
// the filter of GET /v1/credits/balances that Home's credit lines link to.

import { ListCreditBalancesBalance } from './gen/schemas/listCreditBalancesBalance.ts'

const LABELS: Record<ListCreditBalancesBalance, { chip: string; empty: string }> = {
  out: { chip: 'Out of credit', empty: 'No customer is out of credit.' },
  low: { chip: 'Low on credit', empty: 'No customer is low on credit.' },
}

/** The filter a URL value names, or undefined for none or a value the list does not know. */
export function creditBalanceFilter(value: string): ListCreditBalancesBalance | undefined {
  return value === ListCreditBalancesBalance.out || value === ListCreditBalancesBalance.low ? value : undefined
}

/** The chip that shows the filter is on, and the line the table shows when it picks no row. */
export function creditBalanceFilterLabels(filter: ListCreditBalancesBalance): { chip: string; empty: string } {
  return LABELS[filter]
}
