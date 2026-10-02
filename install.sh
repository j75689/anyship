#!/bin/sh
# Install the anyship binary from GitHub releases.
#
#   curl -fsSL https://raw.githubusercontent.com/j75689/anyship/main/install.sh | sh
#
# Environment:
#   ANYSHIP_VERSION        release tag to install, e.g. v0.2.0 (default: latest)
#   ANYSHIP_INSTALL_DIR    where to put the binary (default: /usr/local/bin if
#                          writable, otherwise ~/.local/bin)
#   ANYSHIP_DOWNLOAD_BASE  download from this URL instead of GitHub (mirrors, tests)
#
# The archive is verified against the release's checksums.txt before
# anything is installed.
set -eu

repo="j75689/anyship"
version="${ANYSHIP_VERSION:-latest}"

fail() {
	echo "anyship install: $*" >&2
	exit 1
}

case "$(uname -s)" in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) fail "unsupported OS $(uname -s); download a release archive from https://github.com/$repo/releases" ;;
esac
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) fail "unsupported CPU $(uname -m)" ;;
esac

base="${ANYSHIP_DOWNLOAD_BASE:-}"
if [ -z "$base" ]; then
	if [ "$version" = latest ]; then
		base="https://github.com/$repo/releases/latest/download"
	else
		base="https://github.com/$repo/releases/download/$version"
	fi
fi
asset="anyship_${os}_${arch}.tar.gz"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

download() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO "$2" "$1"
	else
		fail "curl or wget is required"
	fi
}

echo "Downloading $asset ($version)..."
download "$base/$asset" "$tmp/$asset" || fail "could not download $base/$asset"
download "$base/checksums.txt" "$tmp/checksums.txt" || fail "could not download $base/checksums.txt"

expected=$(awk -v f="$asset" '$2 == f { print $1 }' "$tmp/checksums.txt")
[ -n "$expected" ] || fail "checksums.txt has no entry for $asset"
if command -v sha256sum >/dev/null 2>&1; then
	actual=$(sha256sum "$tmp/$asset" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
	actual=$(shasum -a 256 "$tmp/$asset" | awk '{ print $1 }')
else
	fail "sha256sum or shasum is required to verify the download"
fi
[ "$actual" = "$expected" ] || fail "checksum mismatch for $asset (expected $expected, got $actual); not installing"

tar -xzf "$tmp/$asset" -C "$tmp"

dir="${ANYSHIP_INSTALL_DIR:-}"
if [ -z "$dir" ]; then
	if [ -w /usr/local/bin ]; then
		dir=/usr/local/bin
	else
		dir="$HOME/.local/bin"
	fi
fi
mkdir -p "$dir"
cp "$tmp/anyship" "$dir/anyship.tmp"
chmod 755 "$dir/anyship.tmp"
mv "$dir/anyship.tmp" "$dir/anyship"

echo "Installed $("$dir/anyship" --version) to $dir/anyship"
case ":$PATH:" in
*":$dir:"*) ;;
*) echo "Add $dir to your PATH to run anyship from anywhere." ;;
esac
