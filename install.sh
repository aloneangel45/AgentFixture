#!/bin/sh
# AgentFixture Universal POSIX/Bash One-Line Installer (Linux & macOS amd64/arm64)
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/aloneangel45/AgentFixture/main/install.sh | sh
#   AGENTFIXTURE_VERSION=v1.2.0 curl -fsSL https://raw.githubusercontent.com/aloneangel45/AgentFixture/main/install.sh | sh

set -eu

REPO="${AGENTFIXTURE_REPO:-aloneangel45/AgentFixture}"
VERSION="${AGENTFIXTURE_VERSION:-latest}"
BINARY_NAME="agentfixture"

log_info() {
  printf '[AgentFixture] %s\n' "$*"
}

log_err() {
  printf '[AgentFixture] ERROR: %s\n' "$*" >&2
}

detect_os() {
  raw_os="$(uname -s 2>/dev/null || echo unknown)"
  case "$raw_os" in
    Linux|linux)
      echo "linux"
      ;;
    Darwin|darwin)
      echo "darwin"
      ;;
    MINGW*|MSYS*|CYGWIN*)
      log_err "Windows shell detected. Please use PowerShell installer instead:"
      log_err "  irm https://raw.githubusercontent.com/${REPO}/main/install.ps1 | iex"
      exit 1
      ;;
    *)
      log_err "Unsupported operating system: ${raw_os} (supported: Linux, macOS)"
      exit 1
      ;;
  esac
}

detect_arch() {
  raw_arch="$(uname -m 2>/dev/null || echo unknown)"
  case "$raw_arch" in
    x86_64|amd64)
      echo "amd64"
      ;;
    aarch64|arm64)
      echo "arm64"
      ;;
    *)
      log_err "Unsupported CPU architecture: ${raw_arch} (supported: amd64, arm64)"
      exit 1
      ;;
  esac
}

download_file() {
  url="$1"
  dest="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --retry 3 "$url" -o "$dest"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$dest" "$url"
  else
    log_err "Neither 'curl' nor 'wget' is installed."
    exit 1
  fi
}

compute_sha256() {
  target="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$target" | awk '{print tolower($1)}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$target" | awk '{print tolower($1)}'
  elif command -v openssl >/dev/null 2>&1; then
    openssl dgst -sha256 "$target" | awk '{print tolower($NF)}'
  else
    log_err "No SHA-256 tool found (expected sha256sum, shasum, or openssl). Mandatory checksum verification cannot proceed."
    exit 1
  fi
}

extract_expected_sha256() {
  checksums_file="$1"
  asset_name="$2"
  awk -v asset="$asset_name" '
    NF >= 2 {
      fname = $2
      sub(/^\*/, "", fname)
      sub(/^\.\//, "", fname)
      if (fname == asset) {
        print tolower($1)
        exit
      }
    }
  ' "$checksums_file"
}

resolve_base_url() {
  if [ -n "${AGENTFIXTURE_BASE_URL:-}" ]; then
    printf '%s' "${AGENTFIXTURE_BASE_URL%/}"
    return
  fi
  if [ "$VERSION" = "latest" ]; then
    printf 'https://github.com/%s/releases/latest/download' "$REPO"
  else
    case "$VERSION" in
      v*) tag="$VERSION" ;;
      *)  tag="v$VERSION" ;;
    esac
    printf 'https://github.com/%s/releases/download/%s' "$REPO" "$tag"
  fi
}

resolve_install_dir() {
  if [ -n "${AGENTFIXTURE_INSTALL_DIR:-}" ]; then
    printf '%s' "$AGENTFIXTURE_INSTALL_DIR"
    return
  fi
  if [ -w "/usr/local/bin" ] || [ "$(id -u)" -eq 0 ] || command -v sudo >/dev/null 2>&1; then
    printf '/usr/local/bin'
  else
    printf '%s/.local/bin' "$HOME"
  fi
}

main() {
  os_name="$(detect_os)"
  arch_name="$(detect_arch)"
  asset="${BINARY_NAME}-${os_name}-${arch_name}.tar.gz"
  base_url="$(resolve_base_url)"
  install_dir="$(resolve_install_dir)"

  if ! command -v tar >/dev/null 2>&1; then
    log_err "'tar' is required to extract ${asset}."
    exit 1
  fi

  tmp_dir="$(mktemp -d 2>/dev/null || mktemp -d -t agentfixture)"
  trap 'rm -rf "$tmp_dir"' EXIT INT TERM

  archive_path="${tmp_dir}/${asset}"
  checksums_path="${tmp_dir}/checksums.txt"

  log_info "Downloading ${asset} (${os_name}/${arch_name}) from ${base_url}..."
  download_file "${base_url}/${asset}" "$archive_path"

  log_info "Downloading checksums.txt for mandatory SHA-256 verification..."
  download_file "${base_url}/checksums.txt" "$checksums_path"

  expected_sha="$(extract_expected_sha256 "$checksums_path" "$asset")"
  if [ -z "$expected_sha" ]; then
    log_err "Asset '${asset}' not found in checksums.txt. Aborting installation."
    exit 1
  fi

  actual_sha="$(compute_sha256 "$archive_path")"
  if [ "$actual_sha" != "$expected_sha" ]; then
    log_err "SHA-256 checksum mismatch for ${asset}!"
    log_err "  Expected: ${expected_sha}"
    log_err "  Actual:   ${actual_sha}"
    exit 1
  fi
  log_info "SHA-256 verified: ${actual_sha}"

  tar -xzf "$archive_path" -C "$tmp_dir"
  extracted_bin="${tmp_dir}/${BINARY_NAME}"
  if [ ! -f "$extracted_bin" ]; then
    log_err "Expected binary '${BINARY_NAME}' was not found inside ${asset}."
    exit 1
  fi
  chmod +x "$extracted_bin"

  dest_bin="${install_dir%/}/${BINARY_NAME}"
  if [ ! -d "$install_dir" ]; then
    mkdir -p "$install_dir" 2>/dev/null || sudo mkdir -p "$install_dir"
  fi

  if [ -w "$install_dir" ]; then
    mv "$extracted_bin" "$dest_bin"
  elif command -v sudo >/dev/null 2>&1; then
    log_info "Installing to ${dest_bin} via sudo..."
    sudo mv "$extracted_bin" "$dest_bin"
  else
    log_err "Cannot write to ${install_dir} and 'sudo' is unavailable. Set AGENTFIXTURE_INSTALL_DIR to a writable directory."
    exit 1
  fi

  log_info "Installed successfully to ${dest_bin}"
  "$dest_bin" version || true
}

main "$@"
