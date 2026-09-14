//go:build linux || darwin

package utils

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// saveStderr duplicates the current standard error aside and returns a function
// that puts it back.
//
// Without this the first test to redirect stderr keeps it redirected for every
// test after it, and the run's own failure output goes into a temp file that is
// deleted at the end -- a suite that fails silently, which is worse than one
// that fails.
func saveStderr(t *testing.T) {
	t.Helper()
	fd, err := syscall.Dup(int(os.Stderr.Fd()))
	if err != nil {
		t.Fatalf("Failed to save stderr: %v", err)
	}
	saved := os.NewFile(uintptr(fd), "saved-stderr")
	t.Cleanup(func() {
		if err := RedirectStderr(saved); err != nil {
			t.Errorf("Failed to restore stderr: %v", err)
		}
		_ = saved.Close()
	})
}

// The point of the call: what the runtime writes past the logger has to land in
// the log file. Writing to os.Stderr directly is the closest a test can get to
// a panic without taking the process down with it -- both end up at descriptor
// 2, which is the descriptor being moved.
func TestRedirectStderrSendsLaterWritesToTheFile(t *testing.T) {
	saveStderr(t)

	path := filepath.Join(t.TempDir(), "silo.log")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0644)
	if err != nil {
		t.Fatalf("Failed to open the log file: %v", err)
	}
	defer func() { _ = f.Close() }()

	if err := RedirectStderr(f); err != nil {
		t.Fatalf("Failed to redirect stderr: %v", err)
	}

	const msg = "panic: the server fell over\n"
	if _, err := os.Stderr.WriteString(msg); err != nil {
		t.Fatalf("Failed to write to the redirected stderr: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Failed to read the log file back: %v", err)
	}
	if !strings.Contains(string(got), msg) {
		t.Errorf("The log file holds %q, which does not contain the message written to stderr", got)
	}
}

// Closing the log file must not take stderr with it. The reopen path on SIGHUP
// redirects to the new file and closes the old one, and if the descriptor were
// shared rather than duplicated the process would be left with no stderr at
// all -- which is the failure this exists to prevent, arrived at the other way.
func TestRedirectStderrSurvivesTheFileBeingClosed(t *testing.T) {
	saveStderr(t)

	path := filepath.Join(t.TempDir(), "silo.log")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0644)
	if err != nil {
		t.Fatalf("Failed to open the log file: %v", err)
	}
	if err := RedirectStderr(f); err != nil {
		t.Fatalf("Failed to redirect stderr: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Failed to close the log file: %v", err)
	}

	const msg = "still writable\n"
	if _, err := os.Stderr.WriteString(msg); err != nil {
		t.Fatalf("Writing to stderr after closing the log file failed: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Failed to read the log file back: %v", err)
	}
	if !strings.Contains(string(got), msg) {
		t.Errorf("The log file holds %q, which does not contain the message written after the close", got)
	}
}

// A nil file is the shape the caller can reach: the SIGHUP path reassigns logFp
// only when it was already set. Answering with an error rather than reaching
// for a descriptor keeps that from becoming a redirect onto whatever fd the
// nil receiver reports.
func TestRedirectStderrRefusesNoFile(t *testing.T) {
	if err := RedirectStderr(nil); err == nil {
		t.Error("Redirecting stderr to no file succeeded, and should not have")
	}
}
