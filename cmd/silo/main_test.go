package main

import (
	"strings"
	"testing"

	silod "github.com/SiloDrive/silo/fileserver"
	"github.com/SiloDrive/silo/internal/upgrade"
)

// TestNormalizeVersion pins the one format server-info reports, regardless of
// how the binary was built. A development build carries the source default
// ("0.4.1"); a CI build carries `git describe --tags` for the same commit
// ("v0.4.1"). A client comparing versions must not have to handle both.
func TestNormalizeVersion(t *testing.T) {
	cases := map[string]string{
		// The two spellings of the same release, which is the bug this exists
		// for: both must come out identical.
		"0.4.1":  "0.4.1",
		"v0.4.1": "0.4.1",
		"V0.4.1": "0.4.1",

		// git describe on an untagged or dirty tree. The suffix says what is
		// actually running, so only the prefix goes.
		"v0.4.1-3-gabc1234":       "0.4.1-3-gabc1234",
		"v0.4.1-dirty":            "0.4.1-dirty",
		"v0.4.1-3-gabc1234-dirty": "0.4.1-3-gabc1234-dirty",

		// --always with no tag in history gives a bare short SHA. Hex has no
		// "v", so there is nothing to strip, but check it survives anyway.
		"abc1234": "abc1234",

		// A "v" not introducing a number is part of the string, not a prefix.
		"version-two": "version-two",
		"v":           "v",
		"":            "",
	}

	for in, want := range cases {
		if got := normalizeVersion(in); got != want {
			t.Errorf("normalizeVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestHelpNamesEverySubcommandOfUser is the guard for the way this drifts.
//
// "silo user quota" landed with its own usage text updated and the top-level
// help untouched, so the command existed, worked, and was invisible to anybody
// who did not already know it was there. Nothing failed: a help screen is
// prose, and prose does not compile.
//
// The two texts are compared rather than generated from one source because
// they are not the same text — the top-level screen is a survey of the whole
// binary and says less about each command on purpose. What must hold is that
// it omits none of them, which is exactly what a comparison can check and a
// shared constant would have prevented anybody from tuning.
func TestHelpNamesEverySubcommandOfUser(t *testing.T) {
	var help strings.Builder
	printUsage(&help)

	for _, sub := range subcommandsIn(silod.UserUsage) {
		if !strings.Contains(help.String(), "silo user "+sub) {
			t.Errorf("the top-level help never mentions %q, so nobody reading it knows the command exists", "silo user "+sub)
		}
	}
}

// subcommandsIn pulls the verbs out of a usage text: the word after "silo user"
// on every line that has one, ignoring the flag groups that precede it.
func subcommandsIn(usage string) []string {
	seen := map[string]bool{}
	var subs []string
	for _, line := range strings.Split(usage, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "silo" || fields[1] != "user" {
			continue
		}
		for _, f := range fields[2:] {
			// Flags come first in these lines, and the first word that is
			// neither a flag nor a placeholder is the subcommand.
			if strings.HasPrefix(f, "[-") || strings.HasPrefix(f, "-") {
				continue
			}
			if strings.HasPrefix(f, "<") || strings.HasPrefix(f, "[") {
				break
			}
			if !seen[f] {
				seen[f] = true
				subs = append(subs, f)
			}
			break
		}
	}
	return subs
}

// TestUpgradeExitStatus pins the contract --check exists for: a cron or a
// monitoring check wants "is there something to do" as a status, not as prose
// it has to grep.
//
// The case worth being deliberate about is ComparisonUnknown. That is what a
// rate-limited API or an untagged development build produces, and neither is
// news. Reporting it as "upgrade available" would have every such check page
// somebody the first time GitHub's 60-an-hour limit is reached.
func TestUpgradeExitStatus(t *testing.T) {
	cases := []struct {
		check bool
		c     upgrade.Comparison
		want  int
	}{
		{false, upgrade.Behind, 0},
		{false, upgrade.UpToDate, 0},

		{true, upgrade.Behind, 1},
		{true, upgrade.UpToDate, 0},
		{true, upgrade.Development, 0},
		{true, upgrade.Ahead, 0},
		{true, upgrade.ComparisonUnknown, 0},
	}
	for _, c := range cases {
		if got := upgradeExit(c.check, c.c); got != c.want {
			t.Errorf("upgradeExit(%v, %v) = %d, want %d", c.check, c.c, got, c.want)
		}
	}
}

// TestHelpNamesUpgrade. The help screen is the only place a subcommand is
// discoverable, and TestHelpNamesEverySubcommandOfUser above exists because a
// command once shipped working and invisible.
func TestHelpNamesUpgrade(t *testing.T) {
	var help strings.Builder
	printUsage(&help)
	if !strings.Contains(help.String(), "silo upgrade") {
		t.Error("the top-level help never mentions `silo upgrade`, so nobody reading it knows it exists")
	}
}
