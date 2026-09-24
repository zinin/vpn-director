package vpnconfig

import "errors"

// ErrServersSaved marks a failure that came after a subscription file was
// written - or removed - by one of the operations in subops.go: the list is
// out, and only the config it is read against is not. Every other failure
// changed nothing, so an importer says "saved" for this one only.
var ErrServersSaved = errors.New("servers saved")

// savedError is a failure after the file was written. It is ErrServersSaved
// to errors.Is and reads as its cause, which is what the importers show.
type savedError struct{ cause error }

func (e *savedError) Error() string   { return e.cause.Error() }
func (e *savedError) Unwrap() []error { return []error{ErrServersSaved, e.cause} }

// ServersSaved marks err as a failure that came after the file was written.
func ServersSaved(err error) error { return &savedError{cause: err} }
