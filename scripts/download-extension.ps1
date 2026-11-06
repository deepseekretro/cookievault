# CookieVault - one-click download & install the browser extension (Windows)
#
# Usage (PowerShell):
#   irm https://raw.githubusercontent.com/deepseekretro/cookievault/master/scripts/download-extension.ps1 | iex
#   # or:
#   .\scripts\download-extension.ps1 [-InstallDir "C:\path\to\extension"]
#
# Default install dir: %LOCALAPPDATA%\CookieVault\extension
# After running, load the folder as an unpacked extension in Chrome/Edge:
#   chrome://extensions  (enable Developer mode -> Load unpacked)
param(
  [string]$InstallDir = (Join-Path $env:LOCALAPPDATA "CookieVault\extension")
)

$ErrorActionPreference = "Stop"
$Repo = "deepseekretro/cookievault"
$TmpDir = Join-Path ([System.IO.Path]::GetTempPath()) ("cookievault-" + [Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Force -Path $TmpDir | Out-Null

try {
  Write-Host "== CookieVault extension installer =="
  Write-Host "Repo:        $Repo"
  Write-Host "Install dir: $InstallDir"

  $Zip = Join-Path $TmpDir "cookievault-extension.zip"
  $Src = $null

  # 1) Preferred: latest GitHub Release asset "cookievault-extension.zip"
  Write-Host "Checking latest release..."
  try {
    $rel = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" -Headers @{ "User-Agent" = "CookieVault-Installer" }
    $asset = $rel.assets | Where-Object { $_.name -eq "cookievault-extension.zip" } | Select-Object -First 1
    if ($asset) {
      Write-Host "Downloading release asset..."
      Invoke-WebRequest -Uri $asset.browser_download_url -OutFile $Zip
      $Src = Join-Path $TmpDir "release"
      Expand-Archive -Path $Zip -DestinationPath $Src -Force
    }
  } catch {
    Write-Host "No release asset available ($($_.Exception.Message)), falling back to source archive."
  }

  # 2) Fallback: repo source archive (master branch)
  if (-not $Src) {
    Write-Host "Downloading repo archive..."
    Invoke-WebRequest -Uri "https://github.com/$Repo/archive/refs/heads/master.zip" -OutFile $Zip
    $Src = Join-Path $TmpDir "archive"
    Expand-Archive -Path $Zip -DestinationPath $Src -Force
  }

  # 3) Locate the extension folder (contains manifest.json)
  Write-Host "Locating extension..."
  $manifest = Get-ChildItem -Path $Src -Recurse -Filter "manifest.json" |
    Where-Object { $_.FullName -match "extension" } | Select-Object -First 1
  if (-not $manifest) {
    $manifest = Get-ChildItem -Path $Src -Recurse -Filter "manifest.json" | Select-Object -First 1
  }
  if (-not $manifest) { throw "manifest.json not found in download" }
  $ExtSrc = $manifest.DirectoryName

  # 4) Copy to install dir
  if (Test-Path $InstallDir) { Remove-Item $InstallDir -Recurse -Force }
  New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
  Copy-Item "$ExtSrc\*" $InstallDir -Recurse -Force
  Write-Host "Installed to: $InstallDir"

  # 5) Next steps
  Write-Host ""
  Write-Host "Next steps:"
  Write-Host "  1. Open your browser's extensions page:"
  Write-Host "       Chrome: chrome://extensions/"
  Write-Host "       Edge:   edge://extensions/"
  Write-Host "  2. Enable `"Developer mode`" (top-right toggle)."
  Write-Host "  3. Click `"Load unpacked`" and select:"
  Write-Host "       $InstallDir"
  Write-Host "  4. Start the Go server (see repo README), then open the"
  Write-Host "     extension options page and pair with the server's pair code."
  Write-Host ""
  Write-Host "Done."
} finally {
  Remove-Item $TmpDir -Recurse -Force -ErrorAction SilentlyContinue
}
