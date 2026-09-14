//go:build linux

package utils

import (
	"fmt"
	"os"
	"syscall"
)

// RedirectStderr points the process's standard error at f.
//
// The logger already writes to the file; this is for everything that goes
// around the logger. A runtime panic, a stack dump on SIGQUIT, an allocator
// failure -- the runtime writes those to file descriptor 2 directly and there
// is no hook to redirect them in Go. Without this they land on whatever
// terminal started the process, which under a service manager is nowhere, so
// the one message explaining why the server died is the one message not in the
// log.
//
// dup3 rather than dup2, on every Linux rather than only where it is forced:
// linux/arm64 has no dup2 syscall at all, because it was left out when the
// arm64 ABI dropped the syscalls that duplicate others, and Go's syscall
// package follows the kernel -- syscall.Dup2 does not exist to call there. dup3
// takes the same two descriptors plus a flags word, and with no flags set it is
// dup2 exactly. So the portable call is the one that is not the obvious one,
// and using it everywhere costs nothing and removes a split.
func RedirectStderr(f *os.File) error {
	if f == nil {
		return fmt.Errorf("no file to redirect stderr to")
	}
	if err := syscall.Dup3(int(f.Fd()), int(os.Stderr.Fd()), 0); err != nil {
		return fmt.Errorf("redirect stderr to %s: %w", f.Name(), err)
	}
	return nil
}
