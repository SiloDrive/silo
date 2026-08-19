#!/usr/bin/env bash
# Verify that every `file.go:NNN` citation in docs/ still points at what it
# cited. Docs here name code by line, and a line number is a claim that goes
# stale on the next edit above it with nothing to catch it — this has silently
# broken citations three separate times.
#
# The check is heuristic on purpose: it cannot know what a citation meant, so it
# asserts the weaker thing it can check — that the line exists, and that it is
# not blank or a lone closing brace, which is what drift looks like when it
# happens. A citation that has slid onto a different function is not caught, so
# this is a floor rather than a guarantee.
#
# Usage: scripts/check-refs.sh [--verbose]
set -uo pipefail
cd "$(dirname "$0")/.."

verbose=0
[[ "${1:-}" == "--verbose" ]] && verbose=1

fail=0
checked=0

while IFS= read -r hit; do
	doc="${hit%%:*}"
	rest="${hit#*:}"
	docline="${rest%%:*}"
	ref="${rest#*:}"

	path="${ref%:*}"
	num="${ref##*:}"

	# A citation may be repo-relative or a bare basename. Resolve it to exactly
	# one file, and skip it when it is ambiguous — guessing would report a
	# failure against a file the doc never meant.
	mapfile -t matches < <(find . -path ./.git -prune -o -name "$(basename "$path")" -print 2>/dev/null | sed 's|^\./||' | grep -E "(^|/)${path}$")
	if [[ ${#matches[@]} -ne 1 ]]; then
		[[ $verbose -eq 1 ]] && echo "skip  $doc:$docline  $ref (matched ${#matches[@]} files)"
		continue
	fi
	src="${matches[0]}"
	checked=$((checked + 1))

	total=$(wc -l < "$src")
	if (( num > total )); then
		echo "STALE $doc:$docline  $ref  -> past end of file ($total lines)"
		fail=1
		continue
	fi

	target=$(sed -n "${num}p" "$src" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
	if [[ -z "$target" || "$target" == "}" || "$target" == ")" || "$target" == "})" ]]; then
		echo "STALE $doc:$docline  $ref  -> ${target:-<blank line>}"
		fail=1
		continue
	fi
	[[ $verbose -eq 1 ]] && echo "ok    $doc:$docline  $ref  -> ${target:0:60}"
done < <(grep -rnoE '`[A-Za-z0-9_/]+\.go:[0-9]+' docs --include='*.md' | sed 's/`//')

if (( fail )); then
	echo
	echo "$checked citations checked; the ones above no longer land on code."
	echo "Resolve each with: grep -n '<symbol>' <file>"
	exit 1
fi
echo "$checked code citations in docs/ still land on a line of code."
