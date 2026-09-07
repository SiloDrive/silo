package objstore

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// lockFileName is the file inside the data directory whose flock says who owns
// the store. Named for what it is rather than hidden, because an operator who
// finds it should be able to guess what it does.
const lockFileName = "silo.lock"

// ErrDataDirLocked is returned when another process holds the data directory.
var ErrDataDirLocked = errors.New("the data directory is in use by another process")

// DirLock is an exclusive hold on a data directory, released by Release or by
// the process ending.
type DirLock struct {
	f *os.File
}

// LockDataDir takes the exclusive lock on a data directory, for the life of
// the caller.
//
// One writer per data directory is an invariant the store has always had and
// never enforced. Two servers on one directory do not merely race: the second
// reaches loadPackSet, sees the first's sidecar, and recoverPack truncates the
// live pack to its last indexed record while the first is mid-append. The
// second seals what is left, writing a footer at that offset and unlinking the
// sidecar the first still holds open; the first appends past the footer. Next
// start, the missing sidecar reads as "sealed", the trailer check fails, and
// the whole library is ErrPackCorrupt -- including every frame the first
// server had already acknowledged to a client.
//
// So it is taken before anything opens a pack, and it is taken by the offline
// commands too. `gc -delete` and `gc -compact -delete` used to print "stop the
// server first" and hope; the same file makes that a check, because a rewrite
// deletes the pack a running server holds in memory and the next read of a
// frame that moved is a 404 for an object the client was told was stored.
//
// flock rather than a pidfile, because a pidfile is a lock only if every
// reader agrees to honour it and no process ever dies badly. This lock is held
// by the open file description, so the kernel drops it however the process
// ends -- kill -9, OOM, power cut -- and a leftover file is not a lock. The
// pid written inside is for the error message and nothing else: it is read to
// name a holder, never to decide whether one exists.
func LockDataDir(dir string) (*DirLock, error) {
	path := filepath.Join(dir, lockFileName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening the data directory lock %s: %w", path, err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder := holderPID(f)
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s is held by pid %s", ErrDataDirLocked, path, holder)
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}

	// Recorded after the lock, so what is in the file is always the pid that
	// holds it. Best-effort: a directory that cannot be written to is a
	// problem the store will report far more loudly a moment from now, and
	// failing the start over the error message would be the wrong trade.
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return &DirLock{f: f}, nil
}

// Release drops the lock.
//
// The file stays. Removing it would race with another process that has it open
// and is waiting on the flock: it would unlink the inode that process is
// holding, and the next start would create a second one and lock that instead,
// which is two holders and no error. An empty file costs nothing.
func (l *DirLock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		_ = f.Close()
		return fmt.Errorf("unlocking %s: %w", f.Name(), err)
	}
	return f.Close()
}

// holderPID reads the pid out of a lock file for the refusal message. Whatever
// it cannot read is reported as unknown rather than guessed at -- the lock is
// the flock, and this is only the label on it.
func holderPID(f *os.File) string {
	buf := make([]byte, 32)
	n, err := f.ReadAt(buf, 0)
	if n == 0 || (err != nil && !errors.Is(err, io.EOF)) {
		return "unknown"
	}
	pid := strings.TrimSpace(string(buf[:n]))
	if _, err := strconv.Atoi(pid); err != nil {
		return "unknown"
	}
	return pid
}
