#!/usr/bin/env sh
#
# install.sh — one-shot installer for the `instant` CLI.
#
# Usage:
#   curl -sSfL https://instanode.dev/install.sh | sh
#
# What it does:
#   1. Detect the host OS (darwin | linux | windows) and arch (amd64 | arm64).
#   2. Resolve the latest release tag from the GitHub API (or honor an
#      explicit INSTANT_VERSION env var, e.g. v0.2.0).
#   3. Download the matching tar.gz archive from the release page.
#   4. Verify its SHA-256 against the release's checksums.txt.
#   5. Drop `instant` into INSTANT_INSTALL_DIR (default /usr/local/bin).
#
# Why POSIX sh (not bash): the curl-pipe-sh path runs in whatever /bin/sh
# the user has — that's POSIX dash on Debian/Ubuntu, bash on macOS,
# busybox sh on Alpine. Sticking to POSIX keeps the install path
# friction-free everywhere. No arrays, no [[ ]], no `local`.
#
# Why not `go install`: a Go toolchain is a > 200 MB dependency for what
# should be a 30-second install. `go install` is still documented in the
# README as a fallback for users who already have Go.
#
# CLI-MCP-13R2 — closes the BugBash QA round 2 strategic gap: the CLI had
# no release path at all.

set -eu

REPO="InstaNode-dev/cli"
BINARY_NAME="instant"
DEFAULT_INSTALL_DIR="/usr/local/bin"
INSTALL_DIR="${INSTANT_INSTALL_DIR:-$DEFAULT_INSTALL_DIR}"
VERSION="${INSTANT_VERSION:-}"

# ── helpers ─────────────────────────────────────────────────────────────────

# Coloured logging — gracefully degrades on terminals without ANSI support.
if [ -t 1 ] && command -v tput >/dev/null 2>&1 && [ "$(tput colors 2>/dev/null || echo 0)" -ge 8 ]; then
    BOLD="$(tput bold)"
    DIM="$(tput dim)"
    RED="$(tput setaf 1)"
    GREEN="$(tput setaf 2)"
    YELLOW="$(tput setaf 3)"
    RESET="$(tput sgr0)"
else
    BOLD=""; DIM=""; RED=""; GREEN=""; YELLOW=""; RESET=""
fi

info()  { printf "%s==>%s %s\n" "$GREEN$BOLD" "$RESET" "$1"; }
warn()  { printf "%s==>%s %s\n" "$YELLOW$BOLD" "$RESET" "$1" >&2; }
fail()  { printf "%serror:%s %s\n" "$RED$BOLD" "$RESET" "$1" >&2; exit 1; }

# detect_os normalises uname's output into goreleaser's archive naming
# (darwin | linux | windows). MINGW / MSYS / CYGWIN all collapse to
# `windows`; the windows path emits an explicit "use the zip from the
# release page" message because curl-pipe-sh under windows is not
# something we want to surprise users with.
detect_os() {
    uname_out=$(uname -s 2>/dev/null || echo unknown)
    case "$uname_out" in
        Darwin) printf 'darwin' ;;
        Linux)  printf 'linux' ;;
        MINGW*|MSYS*|CYGWIN*|Windows_NT) printf 'windows' ;;
        *) fail "unsupported OS: $uname_out (expected Darwin, Linux, or Windows)" ;;
    esac
}

# detect_arch normalises uname -m into goreleaser's arch names. The
# common Apple Silicon (arm64), Intel (amd64 / x86_64), and Linux/arm64
# variants are covered; 32-bit and esoteric ISAs are explicitly
# rejected (the platform isn't shipped for them).
detect_arch() {
    arch_out=$(uname -m 2>/dev/null || echo unknown)
    case "$arch_out" in
        x86_64|amd64) printf 'amd64' ;;
        arm64|aarch64) printf 'arm64' ;;
        *) fail "unsupported architecture: $arch_out (expected amd64 or arm64)" ;;
    esac
}

# need_cmd checks that a required CLI exists on PATH, with a clear error
# pointing at the missing dependency.
need_cmd() {
    command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1. Please install it and re-run."
}

# resolve_version queries GitHub's release API for the latest tag when
# INSTANT_VERSION is unset. The endpoint returns 200 with `tag_name` in
# JSON; we grep it out with sed (no jq dependency).
resolve_version() {
    if [ -n "$VERSION" ]; then
        printf '%s' "$VERSION"
        return
    fi
    info "Resolving latest release for $REPO..." >&2
    api_url="https://api.github.com/repos/$REPO/releases/latest"
    latest=$(curl -fsSL "$api_url" 2>/dev/null \
        | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
        | head -n1) || true
    if [ -z "$latest" ]; then
        fail "could not resolve latest release for $REPO. Set INSTANT_VERSION=vX.Y.Z and retry."
    fi
    printf '%s' "$latest"
}

# verify_checksum downloads checksums.txt for the release, matches the
# archive name, and re-computes the SHA-256 locally. shasum (BSD/macOS)
# and sha256sum (GNU/Linux) are both supported transparently.
verify_checksum() {
    archive="$1"
    checksum_url="$2"
    archive_base=$(basename "$archive")
    info "Verifying checksum..."
    if ! curl -fsSL "$checksum_url" -o "$archive.checksums"; then
        fail "could not download checksums.txt from $checksum_url"
    fi
    expected=$(awk -v n="$archive_base" '$2 == n {print $1}' "$archive.checksums")
    if [ -z "$expected" ]; then
        fail "no checksum entry for $archive_base in checksums.txt"
    fi
    if command -v sha256sum >/dev/null 2>&1; then
        actual=$(sha256sum "$archive" | awk '{print $1}')
    elif command -v shasum >/dev/null 2>&1; then
        actual=$(shasum -a 256 "$archive" | awk '{print $1}')
    else
        warn "neither sha256sum nor shasum found; skipping checksum verification"
        return
    fi
    if [ "$expected" != "$actual" ]; then
        fail "checksum mismatch: expected $expected, got $actual"
    fi
    info "Checksum OK ($expected)"
}

# install_binary copies the extracted binary into INSTALL_DIR, using
# sudo if the target dir isn't writable by the current user. Permission
# requests are explicit so a curl-pipe-sh user understands the prompt.
install_binary() {
    src="$1"
    dest="$INSTALL_DIR/$BINARY_NAME"
    if [ -w "$INSTALL_DIR" ]; then
        install -m 0755 "$src" "$dest"
    else
        info "Installing to $dest requires sudo..."
        sudo install -m 0755 "$src" "$dest"
    fi
}

# ── main ────────────────────────────────────────────────────────────────────

need_cmd curl
need_cmd tar
need_cmd uname

os=$(detect_os)
if [ "$os" = "windows" ]; then
    fail "Windows is not supported by this script. Download the .zip from https://github.com/$REPO/releases and add instant.exe to your PATH."
fi
arch=$(detect_arch)
version=$(resolve_version)

# Strip the leading "v" — goreleaser archives use the bare semver in
# their filename (e.g. instant_0.2.0_darwin_arm64.tar.gz).
version_no_v="${version#v}"

archive_name="${BINARY_NAME}_${version_no_v}_${os}_${arch}.tar.gz"
release_base="https://github.com/$REPO/releases/download/$version"
archive_url="$release_base/$archive_name"
checksum_url="$release_base/checksums.txt"

info "Detected: $os/$arch"
info "Installing $BINARY_NAME $version from $archive_url"

tmpdir=$(mktemp -d 2>/dev/null || mktemp -d -t instant-install)
trap 'rm -rf "$tmpdir"' EXIT INT TERM

archive_path="$tmpdir/$archive_name"
if ! curl -fsSL "$archive_url" -o "$archive_path"; then
    fail "could not download $archive_url. Check that the release exists at https://github.com/$REPO/releases/tag/$version"
fi

verify_checksum "$archive_path" "$checksum_url"

info "Extracting archive..."
tar -xzf "$archive_path" -C "$tmpdir"

if [ ! -f "$tmpdir/$BINARY_NAME" ]; then
    fail "extracted archive does not contain $BINARY_NAME"
fi

install_binary "$tmpdir/$BINARY_NAME"

info "Installed $BINARY_NAME $version to $INSTALL_DIR/$BINARY_NAME"
printf "%sRun%s '%s --version' to verify.\n" "$DIM" "$RESET" "$BINARY_NAME"

# Sanity-check that INSTALL_DIR is on PATH; warn if not (silent install
# pipelines are no fun to debug).
case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *)
        warn "$INSTALL_DIR is not on your PATH. Add it to your shell profile, e.g.:"
        printf "\n    export PATH=\"%s:\$PATH\"\n\n" "$INSTALL_DIR" >&2
        ;;
esac
