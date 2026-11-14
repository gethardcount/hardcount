package domain

import "errors"

// ErrLeftForNextRun marks work a job left for its next run on purpose: it
// sent nothing, and the row stays where that run finds it (credits that
// could not be applied before a charge, or a payment provider that was
// briefly unavailable). A real-time job logs it and goes on, since its next
// run is a tick away. A test clock's job returns it: an advance that ended
// with the work undone would be ready with it missing, so the advance fails
// and Retry advance does the work.
var ErrLeftForNextRun = errors.New("left for the next run")
