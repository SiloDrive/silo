#!/usr/bin/env bash
# Report what still survives, line for line, from the Seafile Server tree Silo
# was forked out of.
#
# The question this answers is "is this really yours", and it will be asked by
# the first customer's lawyer and by anyone who ever looks at buying the thing.
# A paragraph of explanation is a worse answer than a command they can run
# themselves, so this exists to make the answer reproducible rather than
# remembered.
#
# For every path that existed at the fork point and still exists here, it counts
# the distinct non-trivial lines that appear in both versions. Whitespace is
# stripped and the lines are compared as a set, so shuffling code around does
# not hide it — a function moved to the bottom of the file still counts.
#
# Read the number as a tripwire and not as proof. It over-counts, because
# `if err != nil {` is shared by every Go program ever written and says nothing
# about descent; that is why --verbose exists, and why every count has to be
# eyeballed once before it is believed. It under-counts too, and more seriously:
# a file that was renamed, or reformatted, or whose logic was carried across in
# different words, does not appear here at all. Nothing this prints is evidence
# that something was cleared. It is only evidence of what plainly has not been.
#
# Compares against the working tree rather than HEAD, so a rewrite in progress
# shows up before it is committed.
#
# Then it looks for the name, which the line count would miss: a file rewritten
# from scratch that still says "seafile" in a comment, a path or a config key is
# not cleared, and the count cannot see it.
#
# Exits 0 only when nothing from the fork point survives and nothing outside the
# exemptions names the upstream, so that "the tree is clear" is a thing the shell
# can say rather than a thing we assert.
#
# Usage: scripts/lineage-check.sh [--verbose]
#        LINEAGE_FORK_REF=<ref> scripts/lineage-check.sh
set -uo pipefail
cd "$(dirname "$0")/.."

# The initial import. Everything before it is upstream's and everything after
# it is ours; that is the whole basis of the comparison.
fork="${LINEAGE_FORK_REF:-9cf6605}"

verbose=0
[[ "${1:-}" == "--verbose" ]] && verbose=1

if ! git rev-parse --verify --quiet "$fork^{commit}" >/dev/null; then
	echo "lineage-check: no such commit: $fork" >&2
	exit 2
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# Drop the lines that carry no expression: blanks, lone delimiters, bare comment
# markers. Left in, they are shared by every file in every project and would
# swamp the count they are supposed to inform.
normalise() {
	sed 's/^[[:space:]]*//;s/[[:space:]]*$//' |
		grep -avE '^$|^\}$|^\{$|^\)$|^\($|^//$' |
		sort -u
}

# Paths the line count cannot say anything useful about.
#
# Each of these was read line by line before it was listed here, and each is
# exempt because the lines it shares have one correct spelling rather than
# because they were hard to change. Rewriting them would mean writing a worse
# file in order to word it differently, which is not what clearing the lineage
# means. The exemption is the eyeballing, recorded.
#
# LICENSE.txt is the AGPL, and the AGPL is meant to be reproduced word for word,
# so a line it shares with upstream is the Free Software Foundation's sentence
# rather than anybody's code. What used to matter in that file was the header
# above the licence text, where upstream granted a linking exception in its own
# name; that header is now a statement of Silo's copyright, and the text below it
# was fetched from gnu.org rather than inherited.
#
# .github/workflows/golangci-lint.yml and .gitignore share only the lines the
# tools dictate: `runs-on: ubuntu-latest`, `- uses: actions/checkout@v4`,
# `.DS_Store`, `*.log`. There is no second way to write "run this on Ubuntu" or
# "ignore the file macOS drops in every directory". Everything in either file
# that involved a choice — the concurrency rule, the job names, the baseline
# database negation — is already ours.
exempt() {
	case "$1" in
	LICENSE.txt) return 0 ;;
	.github/workflows/golangci-lint.yml) return 0 ;;
	.gitignore) return 0 ;;
	esac
	return 1
}

surviving=0
total=0
rows=()

while IFS= read -r f; do
	[[ -f "$f" ]] || continue
	exempt "$f" && continue

	git show "$fork:$f" 2>/dev/null | normalise >"$tmp/old" || continue
	normalise <"$f" >"$tmp/new"

	shared=$(comm -12 "$tmp/old" "$tmp/new" | wc -l)
	[[ "$shared" -eq 0 ]] && continue

	here=$(wc -l <"$f")
	theirs=$(wc -l <"$tmp/old")
	surviving=$((surviving + 1))
	total=$((total + shared))
	rows+=("$(printf '%6d  %6d  %6d  %s' "$shared" "$theirs" "$here" "$f")")

	if [[ "$verbose" -eq 1 ]]; then
		echo "### $f"
		comm -12 "$tmp/old" "$tmp/new" | sed 's/^/    /'
		echo
	fi
done < <(git ls-tree -r --name-only "$fork")

status=0

if [[ "$surviving" -eq 0 ]]; then
	echo "No file from $fork still shares a line with the working tree."
else
	printf '%6s  %6s  %6s  %s\n' shared theirs ours path
	printf '%s\n' "${rows[@]}" | sort -rn
	printf '\n%d shared lines across %d files, measured against %s.\n' \
		"$total" "$surviving" "$(git log -1 --format='%h %s' "$fork")"
	echo "Run with --verbose to see which lines, and judge each one."
	status=1
fi

# NOTICE keeps the attribution deliberately, and this script has to be able to
# say what it is looking for. Everything else that names the upstream is a
# leftover.
named=$(git grep -ril seafile -- . ':!NOTICE' ':!scripts/lineage-check.sh')
if [[ -n "$named" ]]; then
	echo
	echo "Files still naming the upstream:"
	echo "$named" | sed 's/^/    /'
	status=1
fi

[[ "$status" -eq 0 ]] && echo "Nothing from $fork survives in the working tree."
exit "$status"
