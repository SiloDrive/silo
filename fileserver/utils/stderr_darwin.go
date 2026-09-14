//go:build darwin

package utils

import (
	"fmt"
	"os"
	"syscall"
)

// RedirectStderr points the process's standard error at f. See the Linux
// implementation for why the server wants this at all.
//
// dup2 here, because dup3 is a Linux call and Darwin does not have it. The two
// differ only in the flags word dup3 added, which this does not use, so the
// behaviour either way is the same: descriptor 2 becomes another name for f.
func RedirectStderr(f *os.File) error {
	if f == nil {
		return fmt.Errorf("no file to redirect stderr to")
	}
	if err := syscall.Dup2(int(f.Fd()), int(os.Stderr.Fd())); err != nil {
		return fmt.Errorf("redirect stderr to %s: %w", f.Name(), err)
	}
	return nil
}
