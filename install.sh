#!/bin/sh
set -e

REPO="voidmind-io/voidmcp"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"

# Detect OS
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$OS" in
  linux)  OS="linux" ;;
  darwin) OS="darwin" ;;
  *)      echo "Unsupported OS: $OS"; exit 1 ;;
esac

# Detect architecture
ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64)  ARCH="amd64" ;;
  aarch64|arm64)  ARCH="arm64" ;;
  *)              echo "Unsupported architecture: $ARCH"; exit 1 ;;
esac

# Get latest version
VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/')
if [ -z "$VERSION" ]; then
  echo "Failed to fetch latest version"
  exit 1
fi

ARCHIVE="voidmcp-${OS}-${ARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/download/${VERSION}/${ARCHIVE}"

echo "Installing VoidMCP ${VERSION} (${OS}/${ARCH})..."

# Download and extract
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

curl -fsSL "$URL" -o "${TMP}/${ARCHIVE}"
tar -xzf "${TMP}/${ARCHIVE}" -C "$TMP"

# Install
mkdir -p "$INSTALL_DIR"
mv "${TMP}/voidmcp" "${INSTALL_DIR}/voidmcp"
chmod +x "${INSTALL_DIR}/voidmcp"

echo "Installed to ${INSTALL_DIR}/voidmcp"

# Check PATH
case ":$PATH:" in
  *":${INSTALL_DIR}:"*) ;;
  *) echo "Add ${INSTALL_DIR} to your PATH if not already:"; echo "  export PATH=\"${INSTALL_DIR}:\$PATH\"" ;;
esac

echo ""
echo "Quick start:"
echo "  claude mcp add --transport stdio voidmcp -- voidmcp serve --stdio"
echo ""
echo "Done."
