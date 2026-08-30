//go:build !linux && !darwin

package diskfree

// available has no implementation here. See ErrUnsupported for why this is an
// error rather than a zero.
func available(string) (int64, error) { return 0, ErrUnsupported }
