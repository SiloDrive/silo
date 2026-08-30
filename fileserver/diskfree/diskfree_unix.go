//go:build linux || darwin

package diskfree

import "syscall"

// available is statfs, with the block count that an ordinary user may spend.
//
// Bavail rather than Bfree: the difference between them is the reserve the
// filesystem keeps for root, which the server does not run as and cannot
// spend. The two field types differ across the platforms this builds for --
// Bsize is int64 on Linux and uint32 on Darwin -- so both are converted rather
// than multiplied as they come.
func available(dir string) (int64, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		return 0, err
	}
	return int64(fs.Bavail) * int64(fs.Bsize), nil
}
