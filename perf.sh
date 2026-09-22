#!/bin/sh
# perf.sh - measure what a lookup costs, registry by registry.
#
# Runs `ldi -V` over a list of references and tabulates what the -verbose
# flag reports: pages fetched, tags seen, candidates considered, platform
# lookups made, and the time spent. One row per reference.
#
#   sh ./perf.sh                       read ./bin/image-list3.txt
#   sh ./perf.sh some-other-list.txt   read another list
#   LDI=./bin/ldi-linux2 sh ./perf.sh   measure another binary
#   CSV=run1.csv sh ./perf.sh          also write a CSV, to diff two runs
#
# The times are wall clock and include network latency, so they describe
# this link as much as the registry. Compare runs with each other rather
# than reading the absolute numbers.
#
# A line may carry a tag filter (image:regex). Quote the regex in the
# SHELL, never in the list file: a quote inside the file becomes part of
# the pattern and the line then silently matches nothing.

set -eu
cd "$(dirname "$0")"

LIST=${1:-./bin/image-list3.txt}

if [ -z "${LDI:-}" ]; then
	if [ -x ./bin/ldi.exe ]; then
		LDI=./bin/ldi.exe
	elif [ -x ./bin/ldi-linux ]; then
		LDI=./bin/ldi-linux
	else
		echo "perf.sh: no binary under ./bin; run build.sh first" >&2
		exit 1
	fi
fi

[ -f "$LIST" ] || { echo "perf.sh: $LIST not found" >&2; exit 1; }

tmp=${TMPDIR:-/tmp}
rows=$tmp/ldi-perf-rows.$$
errf=$tmp/ldi-perf-err.$$
: > "$rows"
trap 'rm -f "$rows" "$errf"' EXIT

echo "# $($LDI --version 2>/dev/null || echo 'ldi version unknown')  ($LDI)"
echo "# list: $LIST"
echo

while IFS= read -r line || [ -n "$line" ]; do
	# Trim surrounding blanks; skip blank and commented lines.
	ref=$(printf '%s' "$line" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')
	case $ref in
		''|'#'*) continue ;;
	esac

	# Quotes in the file would end up inside the tag filter.
	case $ref in
		*\'*|*\"*)
			echo "perf.sh: quotes stripped from '$ref' - fix the list file" >&2
			ref=$(printf '%s' "$ref" | tr -d "\"'")
			;;
	esac

	if out=$("$LDI" -V "$ref" 2>"$errf"); then
		rc=0
	else
		rc=$?
		out=""
	fi

	# "  34 pages, 3354 tags, 60 candidates, 3 lookups, 7.259s"
	stats=$(sed -n \
		's/^[[:space:]]*\([0-9][0-9]*\) page[s]*, \([0-9][0-9]*\) tag[s]*, \([0-9][0-9]*\) candidate[s]*, \([0-9][0-9]*\) lookup[s]*, \(.*\)$/\1\t\2\t\3\t\4\t\5/p' \
		"$errf" | tail -1)
	[ -n "$stats" ] || stats="0	0	0	0	0s"

	if [ "$rc" -eq 0 ]; then
		result=${out##*:}
	else
		# Keep the registry's own words: they say why it failed.
		result=$(tr -d '\r' < "$errf" | grep -v '^[[:space:]]*[0-9][0-9]* page' \
			| sed 's/^ldi: //' | tail -1 | cut -c1-46)
		result="rc=$rc ${result:-no output}"
	fi

	printf '%s\t%s\t%s\n' "$ref" "$stats" "$result" >> "$rows"
	printf '.' >&2
done < "$LIST"

printf '\n\n' >&2

awk -F'\t' '
function toms(t,   n, a) {   # n and a are locals: awk has no other way
	if (t ~ /ms$/)        { sub(/ms$/, "", t);       return t + 0 }
	if (t ~ /(µ|u)s$/)    { sub(/(µ|u)s$/, "", t);   return (t + 0) / 1000 }
	if (t ~ /m[0-9]/)     { n = split(t, a, "m"); sub(/s$/, "", a[2])
	                        return a[1] * 60000 + a[2] * 1000 }
	sub(/s$/, "", t);     return (t + 0) * 1000
}
function human(ms) {
	if (ms >= 1000) return sprintf("%.2fs", ms / 1000)
	return sprintf("%dms", ms)
}
BEGIN {
	fmt = "%-48s %6s %7s %6s %6s %9s  %s\n"
	printf fmt, "IMAGE", "PAGES", "TAGS", "CAND", "LOOK", "TIME", "RESULT"
}
{
	ms = toms($6)
	pages += $2; tags += $3; cand += $4; look += $5; total += ms
	name = $1
	if (length(name) > 48) name = substr(name, 1, 45) "..."
	printf fmt, name, $2, $3, $4, $5, human(ms), $7
	n++
}
END {
	printf fmt, "", "-----", "-----", "-----", "-----", "--------", ""
	printf fmt, n " reference(s)", pages, tags, cand, look, human(total), ""
}' "$rows"

if [ -n "${CSV:-}" ]; then
	{
		echo "image,pages,tags,candidates,lookups,time,result"
		awk -F'\t' '{ printf "\"%s\",%s,%s,%s,%s,%s,\"%s\"\n", $1,$2,$3,$4,$5,$6,$7 }' "$rows"
	} > "$CSV"
	echo
	echo "CSV: $CSV"
fi
