// Package format holds small formatting helpers shared between the CLI and TUI.
package format

import "fmt"

// Bytes renders a byte count as a short human-readable string
// (B / KB / MB / GB / TB / PB), scaling by 1024.
//
// This is the one byte formatter in the tree: the CLI listings, the TUI and
// the gc and backup commands all report sizes through it, so a whole-store
// total and a single file are always in the same units.
func Bytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit && exp < len("KMGTP")-1; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(size)/float64(div), "KMGTP"[exp])
}
