// Package diskfree answers how much room is left on the filesystem holding a
// directory.
//
// It exists because a quota that only counts what Silo believes it stored is
// blind to the one thing that actually stops a server working: the volume
// filling up from outside it. Another tenant, a log that was never rotated, a
// backup somebody dropped beside the data directory -- none of them appear in
// LibraryUsage, and all of them take the database down when the last block
// goes, because SQLite in WAL mode needs room to write before it can commit.
//
// The number is deliberately the *unprivileged* one. Most filesystems keep a
// reserve only root may spend, and reporting that as available would let Silo
// admit a write the process it runs as cannot actually complete.
package diskfree

import "fmt"

// ErrUnsupported is returned on a platform this package cannot ask.
//
// A named error rather than a zero, because zero bytes free is a real answer
// that means the opposite of "I could not find out", and a caller that cannot
// tell them apart refuses every write on a platform it merely does not know
// how to measure.
var ErrUnsupported = fmt.Errorf("free space is not readable on this platform")

// Available reports the bytes still writable on the filesystem holding dir.
//
// dir must exist: this asks about the filesystem a path is on, and a path that
// is not there is on no filesystem. The server's data directory is created
// before anything calls this.
func Available(dir string) (int64, error) {
	n, err := available(dir)
	if err != nil {
		return 0, fmt.Errorf("failed to read free space on %s: %w", dir, err)
	}
	return n, nil
}
