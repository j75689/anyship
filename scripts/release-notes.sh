#!/bin/sh
# Print the CHANGELOG.md section for a version, as GitHub release notes.
#
#   scripts/release-notes.sh v0.3.0
#
# Fails when the changelog has no section for the version, when the section
# is empty, or when its heading has no date yet ("## [0.3.0] - unreleased"),
# so a tag can't publish a release the changelog doesn't describe. Set
# ALLOW_UNRELEASED=1 to preview a section before it is dated.
#
# GitHub renders a line break inside release notes as a line break, so the
# wrapped lines of a paragraph or list item are joined into one. Links to
# files in the repository are pointed at the release's tag, because a relative
# link does not resolve on a release page.
set -eu

[ $# -eq 1 ] || { echo "usage: $0 <version>" >&2; exit 2; }
version=${1#v}
changelog=$(dirname "$0")/../CHANGELOG.md
repo=${GITHUB_SERVER_URL:-https://github.com}/${GITHUB_REPOSITORY:-j75689/anyship}

fail() {
	echo "release notes: $*" >&2
	exit 1
}

heading=$(grep -F -m 1 "## [$version]" "$changelog") || fail "CHANGELOG.md has no section for $version"
case "$heading" in
"## [$version] - "[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]) ;;
*)
	[ "${ALLOW_UNRELEASED:-}" = 1 ] ||
		fail "the heading \"$heading\" has no date; write it as \"## [$version] - YYYY-MM-DD\" before tagging"
	;;
esac

notes=$(awk -v start="## [$version]" '
	function flush() { if (line != "") print line; line = "" }
	index($0, start) == 1 { found = 1; next }
	!found { next }
	/^## \[/ || /^\[[^]]+\]: / { exit }
	fenced { print; if ($0 ~ /^[[:space:]]*```/) fenced = 0; next }
	/^[[:space:]]*```/ { flush(); print; fenced = 1; next }
	/^[[:space:]]*$/ { flush(); print ""; next }
	/^#/ || /^[[:space:]]*([-*+]|[0-9]+\.) / { flush(); line = $0; next }
	{ sub(/^[[:space:]]+/, ""); line = (line == "" ? $0 : line " " $0) }
	END { flush() }
' "$changelog")

[ -n "$(printf '%s' "$notes" | tr -d '[:space:]')" ] || fail "the CHANGELOG.md section for $version is empty"
# Command substitution dropped the trailing blank lines; drop the leading ones.
printf '%s\n' "$notes" | sed '/./,$!d' |
	sed -E 's|\]\(([^):#][^):]*)\)|]('"$repo/blob/v$version"'/\1)|g'
