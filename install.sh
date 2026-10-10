#!/bin/sh
set -e

# ── Anvil Installer ─────────────────────────────────────────
# Usage: curl -fsSL https://raw.githubusercontent.com/anvil-cloud/anvil/master/install.sh | sh

REPO="anvil-cloud/anvil"
INSTALL_DIR="${ANVIL_INSTALL_DIR:-/usr/local/bin}"
BINARY_NAME="anvil"
PROVIDER_NAME="pulumi-resource-anvil"

# ── Detect platform ─────────────────────────────────────────

detect_os() {
  case "$(uname -s)" in
    Linux*)  echo "linux" ;;
    Darwin*) echo "darwin" ;;
    *)       echo "unsupported"; return 1 ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)  echo "amd64" ;;
    arm64|aarch64) echo "arm64" ;;
    *)             echo "unsupported"; return 1 ;;
  esac
}

# ── Fetch latest version ────────────────────────────────────

get_latest_version() {
  if command -v curl > /dev/null 2>&1; then
    curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/'
  elif command -v wget > /dev/null 2>&1; then
    wget -qO- "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/'
  else
    echo "Error: curl or wget is required" >&2
    exit 1
  fi
}

# ── Download helper ─────────────────────────────────────────

download() {
  url="$1"
  output="$2"
  if command -v curl > /dev/null 2>&1; then
    curl -fsSL "$url" -o "$output"
  elif command -v wget > /dev/null 2>&1; then
    wget -qO "$output" "$url"
  fi
}

# ── Main ────────────────────────────────────────────────────

main() {
  OS=$(detect_os)
  ARCH=$(detect_arch)

  if [ "$OS" = "unsupported" ] || [ "$ARCH" = "unsupported" ]; then
    echo "Error: unsupported platform $(uname -s)/$(uname -m)" >&2
    exit 1
  fi

  VERSION="${ANVIL_VERSION:-$(get_latest_version)}"
  if [ -z "$VERSION" ]; then
    echo "Error: could not determine latest version" >&2
    exit 1
  fi

  # Strip leading 'v' for the archive filename
  VERSION_NUM="${VERSION#v}"

  ARCHIVE="anvil_${VERSION_NUM}_${OS}_${ARCH}.tar.gz"
  CHECKSUMS="checksums.txt"
  DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${VERSION}"

  TMPDIR=$(mktemp -d)
  trap 'rm -rf "$TMPDIR"' EXIT

  echo "Installing Anvil ${VERSION} (${OS}/${ARCH})..."

  # Download archive and checksums
  echo "  Downloading ${ARCHIVE}..."
  download "${DOWNLOAD_URL}/${ARCHIVE}" "${TMPDIR}/${ARCHIVE}"

  echo "  Downloading checksums..."
  download "${DOWNLOAD_URL}/${CHECKSUMS}" "${TMPDIR}/${CHECKSUMS}"

  # Verify checksum
  echo "  Verifying checksum..."
  cd "$TMPDIR"
  EXPECTED=$(grep "${ARCHIVE}" "${CHECKSUMS}" | awk '{print $1}')
  if [ -z "$EXPECTED" ]; then
    echo "Error: checksum not found for ${ARCHIVE}" >&2
    exit 1
  fi

  if command -v sha256sum > /dev/null 2>&1; then
    ACTUAL=$(sha256sum "${ARCHIVE}" | awk '{print $1}')
  elif command -v shasum > /dev/null 2>&1; then
    ACTUAL=$(shasum -a 256 "${ARCHIVE}" | awk '{print $1}')
  else
    echo "Warning: no sha256 tool found, skipping checksum verification" >&2
    ACTUAL="$EXPECTED"
  fi

  if [ "$EXPECTED" != "$ACTUAL" ]; then
    echo "Error: checksum mismatch" >&2
    echo "  Expected: ${EXPECTED}" >&2
    echo "  Got:      ${ACTUAL}" >&2
    exit 1
  fi

  # Extract
  echo "  Extracting..."
  tar xzf "${ARCHIVE}"

  # Install
  if [ -w "$INSTALL_DIR" ]; then
    mv "${BINARY_NAME}" "${INSTALL_DIR}/${BINARY_NAME}"
    mv "${PROVIDER_NAME}" "${INSTALL_DIR}/${PROVIDER_NAME}"
  else
    echo "  Installing to ${INSTALL_DIR} (requires sudo)..."
    sudo mv "${BINARY_NAME}" "${INSTALL_DIR}/${BINARY_NAME}"
    sudo mv "${PROVIDER_NAME}" "${INSTALL_DIR}/${PROVIDER_NAME}"
  fi

  chmod +x "${INSTALL_DIR}/${BINARY_NAME}"
  chmod +x "${INSTALL_DIR}/${PROVIDER_NAME}"

  echo ""
  echo "  ✔ Anvil ${VERSION} installed to ${INSTALL_DIR}"
  echo ""
  echo "  Run 'anvil --help' to get started."
}

main