#!/usr/bin/env bash
#
# Install the crossplane-postrender post-renderer binary from a GitHub release.
#
#   # latest release, into ./bin
#   curl -fsSL https://raw.githubusercontent.com/jcogilvie/crossplane-helm-postrender/main/install.sh | bash
#
#   # a pinned version, into a chosen directory
#   curl -fsSL .../install.sh | VERSION=v0.0.1 BIN_DIR=/usr/local/bin bash
#
# Downloads are verified against the release's checksums.txt. Verification is not
# optional: this script is designed to be piped into a shell in CI, where a
# corrupted or substituted archive would otherwise go unnoticed.
#
# Idempotent: if the requested version is already installed, it does nothing. That
# makes it safe to call unconditionally from a Makefile or test harness.

set -euo pipefail

readonly REPO="jcogilvie/crossplane-helm-postrender"
readonly BINARY="crossplane-postrender"

VERSION="${VERSION:-latest}"
BIN_DIR="${BIN_DIR:-./bin}"

log()  { printf '%s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

need() {
    command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"
}

# detect_platform maps uname output onto the GOOS/GOARCH pairs the release
# publishes, and fails explicitly on anything else rather than downloading an
# archive that cannot run.
detect_platform() {
    local os arch
    os="$(uname -s | tr '[:upper:]' '[:lower:]')"
    arch="$(uname -m)"

    case "$os" in
        linux|darwin) ;;
        *) die "unsupported OS: $os (releases cover linux and darwin)" ;;
    esac

    case "$arch" in
        x86_64|amd64) arch="amd64" ;;
        aarch64|arm64) arch="arm64" ;;
        *) die "unsupported architecture: $arch (releases cover amd64 and arm64)" ;;
    esac

    printf '%s_%s' "$os" "$arch"
}

# resolve_version turns "latest" into a concrete tag, so the download URL and the
# checksums file are guaranteed to refer to the same release even if a new one is
# published mid-run.
resolve_version() {
    if [[ "$VERSION" != "latest" ]]; then
        printf '%s' "$VERSION"
        return
    fi

    local tag
    tag="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
        | grep -m1 '"tag_name"' \
        | sed -E 's/.*"tag_name"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/')"

    [[ -n "$tag" ]] || die "cannot determine the latest release of ${REPO}"
    printf '%s' "$tag"
}

main() {
    need curl
    need tar

    local sha_cmd
    if command -v sha256sum >/dev/null 2>&1; then
        sha_cmd="sha256sum"
    elif command -v shasum >/dev/null 2>&1; then
        sha_cmd="shasum -a 256"
    else
        die "neither sha256sum nor shasum found; cannot verify the download"
    fi

    local platform version
    platform="$(detect_platform)"
    version="$(resolve_version)"

    local target="${BIN_DIR}/${BINARY}"
    if [[ -x "$target" ]]; then
        local installed
        installed="$("$target" --version 2>/dev/null || true)"
        if [[ "$installed" == "$version" ]]; then
            log "${BINARY} ${version} already installed at ${target}"
            return 0
        fi
        log "replacing ${BINARY} ${installed:-unknown} with ${version}"
    fi

    local archive="${BINARY}_${version}_${platform}.tar.gz"
    local base="https://github.com/${REPO}/releases/download/${version}"

    local tmp
    tmp="$(mktemp -d)"
    # shellcheck disable=SC2064  # expand tmp now, not at trap time
    trap "rm -rf '$tmp'" EXIT

    log "downloading ${archive}"
    curl -fsSL -o "${tmp}/${archive}" "${base}/${archive}" \
        || die "cannot download ${base}/${archive} (does ${version} publish ${platform}?)"
    curl -fsSL -o "${tmp}/checksums.txt" "${base}/checksums.txt" \
        || die "cannot download checksums for ${version}"

    log "verifying checksum"
    local expected
    expected="$(grep -F " ${archive}" "${tmp}/checksums.txt" | awk '{print $1}')"
    [[ -n "$expected" ]] || die "${archive} is not listed in checksums.txt"

    local actual
    actual="$(cd "$tmp" && $sha_cmd "$archive" | awk '{print $1}')"
    [[ "$actual" == "$expected" ]] \
        || die "checksum mismatch for ${archive}: expected ${expected}, got ${actual}"

    tar -xzf "${tmp}/${archive}" -C "$tmp"

    mkdir -p "$BIN_DIR"
    install -m 0755 "${tmp}/${BINARY}_${version}_${platform}/${BINARY}" "$target"

    log "installed ${BINARY} ${version} to ${target}"

    # A post-renderer is invoked by Helm rather than typed by a human, so point at
    # the two things that most often go wrong on first use.
    if ! docker network inspect crossplane-render >/dev/null 2>&1; then
        log
        log "note: the shared Docker network does not exist yet. Create it once with:"
        log "  docker network create crossplane-render"
    fi
}

main "$@"
