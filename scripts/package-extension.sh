#!/usr/bin/env bash
# Package extension/ into cookievault-extension.zip for attaching to a
# GitHub Release (the download-extension scripts prefer this asset).
set -euo pipefail
cd "$(dirname "$0")/.."
OUT="${1:-cookievault-extension.zip}"
rm -f "$OUT"
if command -v zip >/dev/null 2>&1; then
  (cd extension && zip -qr "../$OUT" .)
elif command -v python3 >/dev/null 2>&1; then
  python3 - <<PY
import zipfile, os
with zipfile.ZipFile("$OUT", "w", zipfile.ZIP_DEFLATED) as z:
    for root, _, files in os.walk("extension"):
        for f in files:
            p = os.path.join(root, f)
            z.write(p, os.path.relpath(p, "extension"))
PY
else
  echo "error: need zip or python3" >&2; exit 1
fi
ls -lh "$OUT"
echo "Attach $OUT to a GitHub Release, e.g.:"
echo "  gh release create v0.1.0 $OUT --title 'CookieVault v0.1.0'"
