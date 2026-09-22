#!/bin/sh
# compare.sh - run two builds of ldi over the same list and diff the answers.
#
# The baseline is the worktree at ../ldi-baseline, built from the tag that
# is currently released. For every reference the script compares what each
# binary wrote to stdout and what it exited with, and reports the time each
# took, so a change can be judged on both counts at once.
#
#   sh ./compare.sh                     ./bin/image-list3.txt
#   sh ./compare.sh some-list.txt       another list
#   OLD=... NEW=... sh ./compare.sh     other binaries
#
# Only lines whose answer changed are called out. A registry that is slow
# is not a difference; a registry that answers differently is.

set -eu
cd "$(dirname "$0")"

LIST=${1:-./bin/image-list3.txt}
OLD=${OLD:-../ldi-baseline/bin/ldi.exe}
NEW=${NEW:-./bin/ldi.exe}

for b in "$OLD" "$NEW"; do
	[ -x "$b" ] || { echo "compare.sh: $b not found or not executable" >&2; exit 1; }
done
[ -f "$LIST" ] || { echo "compare.sh: $LIST not found" >&2; exit 1; }

tmp=${TMPDIR:-/tmp}
errf=$tmp/ldi-cmp-err.$$
trap 'rm -f "$errf"' EXIT

printf 'old: %s  (%s)\n' "$OLD" "$("$OLD" --version 2>/dev/null || echo '?')"
printf 'new: %s  (%s)\n' "$NEW" "$("$NEW" --version 2>/dev/null || echo '?')"
echo

# elapsed BIN REF -> "rc|stdout|time"
one() {
	bin=$1
	ref=$2
	t0=$(date +%s%N 2>/dev/null || echo 0)
	if out=$("$bin" -V "$ref" 2>"$errf"); then rc=0; else rc=$?; out=""; fi
	t1=$(date +%s%N 2>/dev/null || echo 0)
	ms=$(( (t1 - t0) / 1000000 ))
	[ "$t0" != 0 ] || ms=-1
	printf '%s|%s|%s' "$rc" "$out" "$ms"
}

diffs=0
n=0

while IFS= read -r line || [ -n "$line" ]; do
	ref=$(printf '%s' "$line" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')
	case $ref in ''|'#'*) continue ;; esac
	n=$((n + 1))

	a=$(one "$OLD" "$ref")
	b=$(one "$NEW" "$ref")

	arc=${a%%|*}; rest=${a#*|}; aout=${rest%|*}; ams=${rest##*|}
	brc=${b%%|*}; rest=${b#*|}; bout=${rest%|*}; bms=${rest##*|}

	if [ "$arc" = "$brc" ] && [ "$aout" = "$bout" ]; then
		printf 'OK    %-46s %6sms -> %6sms  %s\n' "$ref" "$ams" "$bms" "${bout##*:}"
	else
		diffs=$((diffs + 1))
		printf 'FARK  %-46s %6sms -> %6sms\n' "$ref" "$ams" "$bms"
		printf '        old: rc=%s %s\n' "$arc" "${aout:-<bos>}"
		printf '        new: rc=%s %s\n' "$brc" "${bout:-<bos>}"
	fi
done < "$LIST"

echo
printf '%d reference(s), %d difference(s)\n' "$n" "$diffs"
