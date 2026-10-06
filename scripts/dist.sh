#!/bin/sh
# Build the release archives and checksums.txt into dist/.
#
#   scripts/dist.sh 0.3.0
#
# One archive per platform, named anyship_<os>_<arch> with no version in the
# name so install.sh can use releases/latest/download: a tar.gz holding the
# binary, LICENSE, NOTICE and README.md, or a zip on Windows.
#
# The same commit gives the same bytes: the binaries are built with -trimpath,
# and every archived file carries the commit's time instead of the build's.
set -eu

[ $# -eq 1 ] || { echo "usage: $0 <version>" >&2; exit 2; }
version=${1#v}
platforms="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64"

cd "$(dirname "$0")/.."
export TZ=UTC
stamp=$(git log -1 --format=%cd --date=format-local:%Y%m%d%H%M.%S)
# GNU tar and bsdtar spell "owned by root" differently.
if tar --version | grep -q GNU; then
	owner="--owner=0 --group=0 --numeric-owner"
else
	owner="--uid 0 --gid 0"
fi

rm -rf dist
mkdir dist
for platform in $platforms; do
	os=${platform%/*}
	arch=${platform#*/}
	name=anyship_${os}_${arch}
	bin=anyship
	[ "$os" != windows ] || bin=anyship.exe

	echo "building $name"
	mkdir "dist/$name"
	GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build -trimpath \
		-ldflags "-s -w -X main.version=$version" -o "dist/$name/$bin" ./cmd/anyship
	cp LICENSE NOTICE README.md "dist/$name/"
	touch -t "$stamp" "dist/$name"/*

	if [ "$os" = windows ]; then
		(cd "dist/$name" && zip -q -X "../$name.zip" "$bin" LICENSE NOTICE README.md)
	else
		# shellcheck disable=SC2086 # $owner is two or three flags
		tar -cf - $owner -C "dist/$name" "$bin" LICENSE NOTICE README.md | gzip -n > "dist/$name.tar.gz"
	fi
	rm -rf "dist/$name"
done

cd dist
if command -v sha256sum >/dev/null 2>&1; then
	sha256sum anyship_* > checksums.txt
else
	shasum -a 256 anyship_* > checksums.txt
fi
cat checksums.txt
