#!/bin/sh
# shellcheck shell=sh
#
# clipbeam installer — POSIX sh, no Bash-isms (PLAN §9.1).
#
# One-line install:
#   curl -fsSL https://raw.githubusercontent.com/vybzai/clipbeam-cli/main/install.sh | sh
#
# The apex (https://raw.githubusercontent.com/vybzai/clipbeam-cli/main/install.sh) serves THIS exact script; an
# identical copy is committed at the repo root and shipped inside every release
# archive so it is runnable fully offline from an unpacked tarball.
#
# Behavior (LOCKED, in order):
#   1. OS/arch detect (uname).
#   2. Resolve version (latest GitHub Release, or CLIPBEAM_VERSION override).
#   3. Download clipbeam_<ver>_<os>_<arch>.tar.gz + checksums.txt.
#   4. Verify SHA-256 by default; cosign opt-in via --verify-signature.
#   5. Place into $CLIPBEAM_BIN_DIR else ~/.local/bin (no sudo); /usr/local/bin
#      only when CLIPBEAM_PREFIX=/usr/local AND writable or sudo exists.
#      On macOS, refuse to clobber the existing ClipBeam.app shim unless
#      --replace-shim (PLAN §7.6).
#   6. Print the exact PATH export line if the bindir is not on $PATH.
#   7. Print (do NOT run) the `clipbeam install-skill` hint.
#
# Env knobs: CLIPBEAM_VERSION, CLIPBEAM_BIN_DIR, CLIPBEAM_PREFIX.
# Flags: --verify-signature, --replace-shim.
#
# SPDX-License-Identifier: MIT

set -eu

# ----------------------------------------------------------------------------
# Constants — the CLI ships from vybzai/clipbeam-cli (no separate org).
# ----------------------------------------------------------------------------
ORG="vybzai"
REPO="clipbeam-cli"
GITHUB="https://github.com/${ORG}/${REPO}"
API="https://api.github.com/repos/${ORG}/${REPO}"
# Scheme-less Go module path for the `go install` source-build hint.
MODULE="github.com/${ORG}/${REPO}"
BINARY="clipbeam"

# ----------------------------------------------------------------------------
# Output helpers — diagnostics go to stderr so a piped `| sh` stays clean.
# ----------------------------------------------------------------------------
info() { printf '%s\n' "clipbeam: $*" >&2; }
warn() { printf '%s\n' "clipbeam: warning: $*" >&2; }
err()  { printf '%s\n' "clipbeam: error: $*" >&2; }
die()  { err "$*"; exit 1; }

# ----------------------------------------------------------------------------
# Flags
# ----------------------------------------------------------------------------
VERIFY_SIGNATURE=0
REPLACE_SHIM=0
for arg in "$@"; do
  case "$arg" in
    --verify-signature) VERIFY_SIGNATURE=1 ;;
    --replace-shim)     REPLACE_SHIM=1 ;;
    -h|--help)
      cat >&2 <<'EOF'
clipbeam installer

Usage:
  curl -fsSL https://raw.githubusercontent.com/vybzai/clipbeam-cli/main/install.sh | sh
  sh install.sh [--verify-signature] [--replace-shim]

Flags:
  --verify-signature   Also verify checksums.txt with cosign (keyless, Sigstore).
                       Requires cosign on PATH. Off by default (SHA-256 is the
                       default integrity gate).
  --replace-shim       macOS only: replace an existing ClipBeam.app shim at the
                       install path instead of refusing.

Environment:
  CLIPBEAM_VERSION   Pin a version, e.g. v1.2.3 (default: latest release).
  CLIPBEAM_BIN_DIR   Install directory (default: ~/.local/bin).
  CLIPBEAM_PREFIX    Set to /usr/local to target /usr/local/bin (needs write
                     access or sudo).
EOF
      exit 0
      ;;
    *) die "unknown argument: $arg (try --help)" ;;
  esac
done

# ----------------------------------------------------------------------------
# Dependency probing — we need a downloader and a SHA-256 checker.
# ----------------------------------------------------------------------------
have() { command -v "$1" >/dev/null 2>&1; }

if have curl; then
  DL="curl"
elif have wget; then
  DL="wget"
else
  die "need curl or wget to download"
fi

# download <url> <dest>  — follows redirects (the releases/latest/download fallback)
download() {
  _url="$1"; _dest="$2"
  if [ "$DL" = "curl" ]; then
    curl -fsSL "$_url" -o "$_dest"
  else
    wget -qO "$_dest" "$_url"
  fi
}

# fetch <url> — print body to stdout (used for the GitHub API JSON)
fetch() {
  _url="$1"
  if [ "$DL" = "curl" ]; then
    curl -fsSL "$_url"
  else
    wget -qO- "$_url"
  fi
}

# ----------------------------------------------------------------------------
# 1. OS / arch detect (PLAN §9.1.1)
# ----------------------------------------------------------------------------
detect_os() {
  _u=$(uname -s)
  case "$_u" in
    Darwin) printf 'darwin' ;;
    Linux)  printf 'linux' ;;
    *) die "unsupported OS '$_u'. Build from source: go install ${MODULE}/cmd/clipbeam@latest" ;;
  esac
}

detect_arch() {
  _m=$(uname -m)
  case "$_m" in
    x86_64|amd64)  printf 'amd64' ;;
    arm64|aarch64) printf 'arm64' ;;
    *) die "unsupported arch '$_m'. Build from source: go install ${MODULE}/cmd/clipbeam@latest" ;;
  esac
}

OS=$(detect_os)
ARCH=$(detect_arch)
info "detected ${OS}/${ARCH}"

# The release publishes a darwin UNIVERSAL archive (GoReleaser universal_binaries
# with replace:true removes the per-arch darwin archives), so the asset name on
# macOS uses "universal" regardless of the host arch. Linux keeps amd64/arm64.
if [ "$OS" = "darwin" ]; then
  ARCH_ASSET="universal"
else
  ARCH_ASSET="$ARCH"
fi

# ----------------------------------------------------------------------------
# SHA-256 checker selection (macOS shasum vs Linux sha256sum) — PLAN §9.1.4
# ----------------------------------------------------------------------------
sha_check() {
  # sha_check <checksums-file>  — run in the dir holding the archive.
  if have sha256sum; then
    sha256sum -c "$1"
  elif have shasum; then
    shasum -a 256 -c "$1"
  else
    die "need sha256sum or shasum to verify the download"
  fi
}

# ----------------------------------------------------------------------------
# 2. Resolve version (PLAN §9.1.2)
#    CLIPBEAM_VERSION overrides; else the GitHub API "latest"; the asset URL
#    uses the releases/latest/download redirect so a rate-limited shared NAT
#    egress (60 unauthenticated API calls/hr) still installs.
# ----------------------------------------------------------------------------
VERSION="${CLIPBEAM_VERSION:-}"
if [ -z "$VERSION" ]; then
  info "resolving latest release"
  # Parse the tag_name out of the API JSON without jq (best-effort; if the API
  # is rate-limited we fall back to the latest/download redirect below).
  VERSION=$(fetch "${API}/releases/latest" 2>/dev/null \
    | grep -m1 '"tag_name"' \
    | sed -e 's/.*"tag_name"[[:space:]]*:[[:space:]]*"//' -e 's/".*//' \
    || true)
fi

# ----------------------------------------------------------------------------
# 3. Download archive + checksums (PLAN §9.1.3)
#    If VERSION is known: tagged download path, asset name built directly.
#    If VERSION is empty (API rate-limited): latest/download redirect; the
#    stable checksums.txt yields the versioned asset name (the archive name
#    embeds the version, so it cannot be guessed without it).
# ----------------------------------------------------------------------------
TMP=$(mktemp -d 2>/dev/null || mktemp -d -t clipbeam)
trap 'rm -rf "$TMP"' EXIT INT TERM

CHECKSUMS="${TMP}/checksums.txt"

if [ -n "$VERSION" ]; then
  # The release TAG carries a leading "v" (v0.1.0) and names the download dir,
  # but GoReleaser strips it from the archive name ({{.Version}} => 0.1.0), so the
  # asset filename uses the v-stripped version. (POSIX ${VERSION#v} drops one
  # leading "v" if present.)
  ASSET="${BINARY}_${VERSION#v}_${OS}_${ARCH_ASSET}.tar.gz"
  BASE="${GITHUB}/releases/download/${VERSION}"
  info "installing ${BINARY} ${VERSION}"
  download "${BASE}/checksums.txt" "$CHECKSUMS" \
    || die "checksums download failed: ${BASE}/checksums.txt"
else
  # Latest-redirect fallback (PLAN §9.1.2): avoids the 60/hr unauthenticated API
  # rate limit. The archive name embeds the version (GoReleaser always versions
  # archives), which we cannot know up front — but checksums.txt has a STABLE
  # name at releases/latest/download, so fetch it and read our versioned asset
  # name out of it.
  warn "GitHub API unavailable or rate-limited; using latest-release redirect"
  BASE="${GITHUB}/releases/latest/download"
  download "${BASE}/checksums.txt" "$CHECKSUMS" \
    || die "checksums download failed: ${BASE}/checksums.txt"
  ASSET=$(grep -oE "${BINARY}_[^ ]*_${OS}_${ARCH_ASSET}\.tar\.gz" "$CHECKSUMS" \
    | head -n1 || true)
  [ -n "$ASSET" ] \
    || die "could not find a ${OS}/${ARCH_ASSET} archive in the latest checksums.txt"
fi

ARCHIVE="${TMP}/${ASSET}"

info "downloading ${ASSET}"
download "${BASE}/${ASSET}" "$ARCHIVE" \
  || die "download failed: ${BASE}/${ASSET}"

# ----------------------------------------------------------------------------
# 4. Verify — SHA-256 by default (PLAN §9.1.4). Mismatch => abort, no install.
# ----------------------------------------------------------------------------
info "verifying SHA-256"
(
  cd "$TMP"
  # checksums.txt lists every asset; -c skips files that are absent ("--ignore-
  # missing" is not portable to macOS shasum, so filter to our asset line).
  grep " ${ASSET}\$" checksums.txt > one_checksum.txt 2>/dev/null \
    || die "no SHA-256 entry for ${ASSET} in checksums.txt"
  sha_check one_checksum.txt
) || die "SHA-256 verification FAILED — refusing to install"
info "SHA-256 OK"

# 4b. Optional cosign signature verification (PLAN §9.1.4, §9.3)
if [ "$VERIFY_SIGNATURE" -eq 1 ]; then
  if ! have cosign; then
    die "--verify-signature requires cosign on PATH (install: https://docs.sigstore.dev)"
  fi
  info "verifying cosign signature (keyless / Sigstore)"
  download "${BASE}/checksums.txt.sig" "${TMP}/checksums.txt.sig" \
    || die "signature download failed"
  download "${BASE}/checksums.txt.pem" "${TMP}/checksums.txt.pem" \
    || die "certificate download failed"
  cosign verify-blob \
    --certificate "${TMP}/checksums.txt.pem" \
    --signature "${TMP}/checksums.txt.sig" \
    --certificate-identity-regexp "https://github\.com/vybzai/clipbeam-cli" \
    --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
    "$CHECKSUMS" \
    || die "cosign verification FAILED — refusing to install"
  info "cosign signature OK"
fi

# ----------------------------------------------------------------------------
# 5. Extract + place (PLAN §9.1.5)
# ----------------------------------------------------------------------------
info "extracting"
tar -xzf "$ARCHIVE" -C "$TMP" || die "extract failed"
[ -f "${TMP}/${BINARY}" ] || die "archive did not contain a '${BINARY}' binary"
chmod 755 "${TMP}/${BINARY}"

# Resolve the destination bindir.
#   CLIPBEAM_BIN_DIR wins; else CLIPBEAM_PREFIX=/usr/local -> /usr/local/bin
#   (only if writable or sudo exists); else ~/.local/bin.
if [ -n "${CLIPBEAM_BIN_DIR:-}" ]; then
  BIN_DIR="$CLIPBEAM_BIN_DIR"
  USE_SUDO=0
elif [ "${CLIPBEAM_PREFIX:-}" = "/usr/local" ]; then
  BIN_DIR="/usr/local/bin"
  if [ -w "$BIN_DIR" ] || { [ ! -e "$BIN_DIR" ] && [ -w /usr/local ]; }; then
    USE_SUDO=0
  elif have sudo; then
    warn "/usr/local/bin needs elevation; will use sudo"
    USE_SUDO=1
  else
    die "CLIPBEAM_PREFIX=/usr/local but /usr/local/bin is not writable and sudo is unavailable"
  fi
else
  BIN_DIR="${HOME}/.local/bin"
  USE_SUDO=0
fi

DEST="${BIN_DIR}/${BINARY}"

# macOS shim refusal (PLAN §7.6): the shipped ClipBeam.app installs a POSIX-sh
# shim to ~/.local/bin/clipbeam. Refuse to clobber it unless --replace-shim.
if [ "$OS" = "darwin" ] && [ -e "$DEST" ] && [ "$REPLACE_SHIM" -eq 0 ]; then
  if head -n 4 "$DEST" 2>/dev/null | grep -q 'POSIX sh CLI shim'; then
    die "refusing to overwrite the ClipBeam.app shim at ${DEST}.
       The Go CLI is a superset of the shim's verbs; pass --replace-shim to replace it,
       or set CLIPBEAM_BIN_DIR to install elsewhere."
  fi
fi

run() {
  if [ "${USE_SUDO:-0}" -eq 1 ]; then
    sudo "$@"
  else
    "$@"
  fi
}

info "installing to ${DEST}"
run mkdir -p "$BIN_DIR" || die "could not create ${BIN_DIR}"
# Install to a temp name in the same dir, then atomic mv (avoids EXDEV and a
# half-written binary if the copy is interrupted).
TMPDEST="${DEST}.tmp.$$"
run cp "${TMP}/${BINARY}" "$TMPDEST" || die "copy failed"
run chmod 755 "$TMPDEST"
run mv -f "$TMPDEST" "$DEST" || die "install failed"
info "installed ${BINARY}"

# ----------------------------------------------------------------------------
# 6. PATH hint (PLAN §9.1.6) — never edit rc files silently in the curl|sh path.
# ----------------------------------------------------------------------------
case ":${PATH}:" in
  *":${BIN_DIR}:"*) : ;;  # already on PATH
  *)
    SHELL_NAME=$(basename "${SHELL:-sh}")
    case "$SHELL_NAME" in
      zsh)  RC="$HOME/.zshrc" ;;
      bash) RC="$HOME/.bashrc" ;;
      *)    RC="$HOME/.profile" ;;
    esac
    warn "${BIN_DIR} is not on your PATH. Add it for your shell (${SHELL_NAME}):"
    # shellcheck disable=SC2016  # the single-quoted $PATH is intentional: we print a literal line for the user to paste
    printf '\n  echo '\''export PATH="%s:$PATH"'\'' >> %s\n\n' "$BIN_DIR" "$RC" >&2
    ;;
esac

# ----------------------------------------------------------------------------
# 7. Skill hint (PLAN §9.1.7) — print, never auto-run.
# ----------------------------------------------------------------------------
info "done. Next steps:"
printf '\n  %s setup user@host   # bootstrap a remote box over SSH\n' "$BINARY" >&2
printf '  %s install-skill     # install the agent skill for Claude/Codex\n\n' "$BINARY" >&2
