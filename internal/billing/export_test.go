package billing

// RunBillingHalf runs one billing tick body, as the leader gate does, for the
// external real-Postgres tests (package billing_test).
var RunBillingHalf = (*Scheduler).runBillingHalf

// NewStripeTaxFake starts a Stripe Tax API double for the external
// real-Postgres tests. See stripeTaxFake.
var NewStripeTaxFake = newStripeTaxFake

// StripeTaxFakeRequest is one calculation request the fake recorded.
type StripeTaxFakeRequest = stripeTaxFakeRequest
