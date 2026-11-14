package domain

import "errors"

// ErrLeftForNextRun marks a row a job's run did not finish and leaves for a
// later run, where the row stays. The error it is joined with says what is
// left and what to do. A real-time job logs it and goes on, since its next run
// comes on its own. A test clock's job returns it: an advance that ended with
// the row undone would be ready with that work missing, so the advance fails
// and Retry advance runs the job again.
var ErrLeftForNextRun = errors.New("left for the next run")
