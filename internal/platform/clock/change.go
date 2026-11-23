package clock

import (
	"context"
	"fmt"

	"github.com/gethardcount/hardcount/internal/errs"
)

// While a test clock advances, a request that changes one of its customers is
// refused (409 clock_advancing): the customer, its subscriptions, invoices,
// credit notes and credits, and usage an operator backfills or releases. The
// advance bills the customer's records as the clock's time moves, and a
// change made meanwhile would be worked out at a time the advance has not
// finished. Reads go on, as do usage an app or a proxy sends, payments
// through Stripe, the public pages, the advance's own jobs, and price
// changes, which take effect after the advance (their stamp).
//
// The API's door marks a request that changes a customer's data
// (MarkChange). A reader of a record's clock asks RefuseIfAdvancing when it
// reads the clock for the request's decision, and Decide returns that
// refusal as it is, with a context that no longer carries the mark: only the
// request's own decision is refused, never the work it calls or starts.

type changeKey struct{}

// MarkChange marks ctx as a request that changes a customer's data. The
// API's door calls it, and nothing else does.
func MarkChange(ctx context.Context) context.Context {
	return context.WithValue(ctx, changeKey{}, true)
}

// WithoutChangeMark is ctx without the mark: for work a request starts after
// it answers, which decides each record's time on its own, as a job does,
// and is never refused.
func WithoutChangeMark(ctx context.Context) context.Context {
	if marked, _ := ctx.Value(changeKey{}).(bool); !marked {
		return ctx
	}
	return context.WithValue(ctx, changeKey{}, false)
}

// codeClockAdvancing is the refusal's API code.
const codeClockAdvancing = "clock_advancing"

// RefuseIfAdvancing is the refusal of a marked request whose record is on the
// clock named name, when that clock is advancing; nil otherwise.
func RefuseIfAdvancing(ctx context.Context, name string, advancing bool) error {
	if marked, _ := ctx.Value(changeKey{}).(bool); !marked || !advancing {
		return nil
	}
	return errs.InvalidState(fmt.Sprintf("Test clock %q is advancing, so its customers cannot be changed until it is ready. Try again then.", name)).
		WithCode(codeClockAdvancing)
}

// IsAdvancingRefusal reports whether err is RefuseIfAdvancing's refusal. A
// caller that answers a failed decision with its own error passes this one
// through as it is.
func IsAdvancingRefusal(err error) bool {
	return errs.Code(err) == codeClockAdvancing
}
