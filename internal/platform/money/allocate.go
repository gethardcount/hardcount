package money

// AllocateLIFO fills `total` into buckets in the order given, putting up to
// `caps[i]` into each bucket before spilling to the next. The caller passes
// the buckets newest first. It returns the allocation and the remainder that
// could not be placed.
//
// It is the one split for credit on unused prepaid time
// (domain.AllocateUnusedCredit, ADR-118). The newest charging invoice is
// credited first, reversing its own tax, and older invoices only take what
// the newest has no room for. That holds for seats too. Units look
// fungible, but Stripe credits a decrease "based on the last price billed".
// A split by subtotal would send most of the credit to a renewal invoice
// whose subtotal carries the previous period's usage.
func AllocateLIFO(total int64, caps []int64) ([]int64, int64) {
	out := make([]int64, len(caps))
	remaining := total
	for i := 0; i < len(caps) && remaining > 0; i++ {
		give := caps[i]
		if give < 0 {
			give = 0
		}
		if give > remaining {
			give = remaining
		}
		out[i] = give
		remaining -= give
	}
	if remaining < 0 {
		remaining = 0
	}
	return out, remaining
}
