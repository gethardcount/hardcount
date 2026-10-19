package billing

// RunBillingHalf runs one billing tick body, as the leader gate does, for the
// external real-Postgres tests (package billing_test).
var RunBillingHalf = (*Scheduler).runBillingHalf
