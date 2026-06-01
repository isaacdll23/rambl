#!/bin/sh
# rambl installer — fetches the right prebuilt binary for your platform.
#
#   curl -fsSL https://raw.githubusercontent.com/isaacdll23/rambl/main/install.sh | sh
#
# Environment overrides:
#   VERSION   release tag to install (default: latest), e.g. VERSION=v0.1.0
#   BINDIR    install directory       (default: /usr/local/bin)
#
# rambl drives Claude Code, so after install you still need the `claude` CLI
# (logged in with a Pro/Max plan) and git on PATH.
set -eu

REPO="isaacdll23/rambl"
BIN="rambl"

info() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
err()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

# --- platform detection -----------------------------------------------------
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) err "unsupported architecture: $arch" ;;
esac
case "$os" in
	linux | darwin) ;;
	*) err "unsupported OS: $os (linux and darwin only)" ;;
esac

# --- downloader (curl or wget) ----------------------------------------------
if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL "$1"; }
	fetch_to() { curl -fsSL -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -qO- "$1"; }
	fetch_to() { wget -qO "$2" "$1"; }
else
	err "need curl or wget on PATH"
fi

# --- resolve version --------------------------------------------------------
VERSION="${VERSION:-}"
if [ -z "$VERSION" ]; then
	VERSION=$(fetch "https://api.github.com/repos/$REPO/releases/latest" \
		| grep -m1 '"tag_name"' \
		| sed -E 's/.*"tag_name" *: *"([^"]+)".*/\1/')
	[ -n "$VERSION" ] || err "could not resolve latest version; set VERSION=vX.Y.Z"
fi

ver="${VERSION#v}"
asset="${BIN}_${ver}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$VERSION"

info "installing $BIN $VERSION ($os/$arch)"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fetch_to "$base/$asset" "$tmp/$asset" || err "download failed: $base/$asset"

# --- verify checksum (fail closed if checksums are present) ------------------
if fetch_to "$base/checksums.txt" "$tmp/checksums.txt" 2>/dev/null; then
	if command -v sha256sum >/dev/null 2>&1; then
		sumcmd="sha256sum"
	elif command -v shasum >/dev/null 2>&1; then
		sumcmd="shasum -a 256"
	else
		sumcmd=""
	fi
	if [ -n "$sumcmd" ]; then
		want=$(grep " ${asset}\$" "$tmp/checksums.txt" | awk '{print $1}')
		got=$(cd "$tmp" && $sumcmd "$asset" | awk '{print $1}')
		[ -n "$want" ] && [ "$want" = "$got" ] || err "checksum verification failed for $asset"
		info "checksum verified"
	else
		info "no sha256 tool found; skipping checksum verification"
	fi
fi

tar -xzf "$tmp/$asset" -C "$tmp"
chmod +x "$tmp/$BIN"

# --- install ----------------------------------------------------------------
BINDIR="${BINDIR:-/usr/local/bin}"
if mkdir -p "$BINDIR" 2>/dev/null && [ -w "$BINDIR" ]; then
	mv "$tmp/$BIN" "$BINDIR/$BIN"
elif command -v sudo >/dev/null 2>&1; then
	info "writing $BINDIR (requires sudo)"
	sudo mkdir -p "$BINDIR"
	sudo mv "$tmp/$BIN" "$BINDIR/$BIN"
else
	err "cannot write $BINDIR and sudo is unavailable; re-run with BINDIR=<writable path>"
fi

info "installed $BINDIR/$BIN"
"$BINDIR/$BIN" version 2>/dev/null || true

case ":$PATH:" in
	*":$BINDIR:"*) ;;
	*) info "note: $BINDIR is not on your PATH — add it to use \`$BIN\` directly" ;;
esac
