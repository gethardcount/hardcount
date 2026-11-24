package domain

import "errors"

// ErrLeftForNextRun marks a row a job's run did not finish and leaves for a
// later run, where the row stays. The error it is joined with says what is
// left and what to do. A job logs it and goes on, since its next run comes on
// its own.
var ErrLeftForNextRun = errors.New("left for the next run")
