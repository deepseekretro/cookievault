#!/usr/bin/env bash
# CookieVault - one-click download & install the browser extension (Linux/macOS)
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/deepseekretro/cookievault/master/scripts/download-extension.sh | bash
#   # or:
#   ./scripts/download-extension.sh [install_dir]
#
# Default install dir: ~/.local/share/cookievault/extension
# After running, load the folder as an unpacked extension in Chrome/Edge:
#   chrome://extensions  (enable Developer mode -> Load unpacked)
set -euo pipefail

REPO="deepseekretro/cookievault"
INSTALL_DIR="${1:-$HOME/.local/share/cookievault/extension}"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

dl() {  # dl <url> <out>
  if command -v curl >/dev/null 2>&1; then
    curl -fL --retry 3 -o "$2" "$1"
  else
    wget -O "$2" "$1"
  fi
}

extract() {  # extract <zip> <dest>
  mkdir -p "$2"
  if command -v unzip >/dev/null 2>&1; then
    unzip -q "$1" -d "$2"
  elif command -v python3 >/dev/null 2>&1; then
    python3 -m zipfile -e "$1" "$2"
  else
    echo "error: need unzip or python3 to extract the zip" >&2; exit 1
  fi
}

echo "== CookieVault extension installer =="
echo "Repo:        $REPO"
echo "Install dir: $INSTALL_DIR"

ZIP="$TMP_DIR/cookievault-extension.zip"
SRC=""

# 1) Preferred: latest GitHub Release asset "cookievault-extension.zip"
echo "Checking latest release..."
REL_JSON="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null || true)"
if [ -n "$REL_JSON" ]; then
  ASSET_URL="$(printf '%s' "$REL_JSON" | python3 -c 'import sys,json
try:
    d=json.load(sys.stdin)
    for a in d.get("assets",[]):
        if a.get("name")=="cookievault-extension.zip":
            print(a["browser_download_url"]); break
except Exception: pass' 2>/dev/null || true)"
  if [ -n "$ASSET_URL" ]; then
    echo "Downloading release asset..."
    dl "$ASSET_URL" "$ZIP" && SRC="$TMP_DIR/release" && extract "$ZIP" "$SRC"
  fi
fi

# 2) Fallback: repo source archive (master branch)
if [ -z "$SRC" ]; then
  echo "No release asset found, downloading repo archive..."
  dl "https://github.com/$REPO/archive/refs/heads/master.zip" "$ZIP"
  SRC="$TMP_DIR/archive"
  extract "$ZIP" "$SRC"
fi

# 3) Locate the extension folder (contains manifest.json)
echo "Locating extension..."
MANIFEST="$(find "$SRC" -name manifest.json -path '*extension*' 2>/dev/null | head -1)"
if [ -z "$MANIFEST" ]; then
  MANIFEST="$(find "$SRC" -name manifest.json 2>/dev/null | head -1)"
fi
[ -n "$MANIFEST" ] || { echo "error: manifest.json not found in download" >&2; exit 1; }
EXT_SRC="$(dirname "$MANIFEST")"

mkdir -p "$INSTALL_DIR"
if command -v rsync >/dev/null 2>&1; then
  rsync -a --delete "$EXT_SRC"/ "$INSTALL_DIR"/
else
  cp -R "$EXT_SRC"/. "$INSTALL_DIR"/
fi
echo "Installed to: $INSTALL_DIR"

# 4) Next steps
cat <<EOF

Next steps:
  1. Open your browser's extensions page:
       Chrome: chrome://extensions/
       Edge:   edge://extensions/
  2. Enable "Developer mode" (top-right toggle).
  3. Click "Load unpacked" and select:
       $INSTALL_DIR
  4. Start the Go server (see repo README), then open the
     extension options page and pair with the server's pair code.

Done.
EOF
