package security

import "errors"

// Outcome is what became of a scan Job's call to pergent.sh: its posted
// result, or its source download. The engine returns it and the callback
// answers by it.
type Outcome int

const (
	// Stored: the result was taken (also one that could not be used: the
	// scan then failed), or the source is served.
	Stored Outcome = iota + 1
	// Conflict: the scan is not running.
	Conflict
	// Unauthorized: no such scan, or the token is not the scan's.
	Unauthorized
)

// ErrNoSource is the error of a source download the platform did not give:
// no reader for the instance, a repository or commit the review bot or App
// can't read, or a platform error.
var ErrNoSource = errors.New("the platform did not give the source archive")
