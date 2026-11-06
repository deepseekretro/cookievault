# CookieVault - one-click download & install the browser extension (Windows)
#
# Usage (PowerShell):
#   irm https://raw.githubusercontent.com/deepseekretro/cookievault/master/scripts/download-extension.ps1 | iex
#   # or:
#   .\scripts\download-extension.ps1 [-InstallDir "C:\path\to\extension"] [-LaunchBrowser]
#
# Default install dir: %LOCALAPPDATA%\CookieVault\extension
#
# The script auto-detects the default Edge installation:
#   - Edge exe path   (registry App Paths / Program Files / LocalAppData)
#   - Edge User Data  (%LOCALAPPDATA%\Microsoft\Edge\User Data)
#   - Edge profile    (Default, or the profile marked last-used in Local State)
#   - Extensions dir  (<profile>\Extensions)
# and, with -LaunchBrowser, opens edge://extensions/ in the detected Edge
# so the unpacked extension folder can be loaded immediately.
# (Chrome is detected the same way as a fallback.)
param(
  [string]$InstallDir = (Join-Path $env:LOCALAPPDATA "CookieVault\extension"),
  [string]$EdgeExe = "",
  [string]$UserDataDir = "",
  [switch]$LaunchBrowser
)

$ErrorActionPreference = "Stop"
$Repo = "deepseekretro/cookievault"

# ---------------------------------------------------------------------------
# Browser detection
# ---------------------------------------------------------------------------
function Get-ExeVersion([string]$Path) {
  try { return (Get-Item $Path).VersionInfo.FileVersion } catch { return "" }
}

function Find-BrowserExe([string[]]$AppPathKeys, [string[]]$CandidatePaths) {
  # 1) Registry: App Paths (per-machine, then per-user)
  foreach ($key in $AppPathKeys) {
    foreach ($root in @("HKLM:\", "HKCU:\")) {
      try {
        $p = Get-ItemProperty -Path (Join-Path $root $key) -ErrorAction Stop
        $exe = $p.'(default)'
        if ($exe -and (Test-Path $exe)) { return $exe }
      } catch { }
    }
  }
  # 2) Well-known install locations
  foreach ($p in $CandidatePaths) {
    if ($p -and (Test-Path $p)) { return $p }
  }
  return ""
}

function Get-ProfileDir([string]$UserData) {
  if (-not (Test-Path $UserData)) { return "" }
  # Preferred: profile marked last-used in "Local State"
  try {
    $state = Get-Content (Join-Path $UserData "Local State") -Raw | ConvertFrom-Json
    $lastUsed = $state.profile.last_used
    if ($lastUsed) {
      $candidate = Join-Path $UserData $lastUsed
      if (Test-Path $candidate) { return $candidate }
    }
  } catch { }
  # Fallback: Default, else first Profile N
  $def = Join-Path $UserData "Default"
  if (Test-Path $def) { return $def }
  $p = Get-ChildItem $UserData -Directory -Filter "Profile *" | Sort-Object Name | Select-Object -First 1
  if ($p) { return $p.FullName }
  return ""
}

function Get-BrowserInfo([string]$Name, [string]$Exe, [string]$DataDir) {
  if (-not $Exe) { return $null }
  $profileDir = Get-ProfileDir $DataDir
  return [pscustomobject]@{
    Name          = $Name
    ExePath       = $Exe
    Version       = Get-ExeVersion $Exe
    UserDataDir   = $DataDir
    ProfileDir    = $profileDir
    ExtensionsDir = if ($profileDir) { Join-Path $profileDir "Extensions" } else { "" }
  }
}

function Detect-DefaultBrowser {
  # --- Microsoft Edge (preferred) ---
  $edgeExe = $EdgeExe
  if (-not $edgeExe) {
    $edgeExe = Find-BrowserExe `
      @("SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\msedge.exe") `
      @(
        "${env:ProgramFiles(x86)}\Microsoft\Edge\Application\msedge.exe",
        "$env:ProgramFiles\Microsoft\Edge\Application\msedge.exe",
        "$env:LOCALAPPDATA\Microsoft\Edge\Application\msedge.exe"
      )
  }
  $edgeData = $UserDataDir
  if (-not $edgeData) { $edgeData = Join-Path $env:LOCALAPPDATA "Microsoft\Edge\User Data" }
  $edge = Get-BrowserInfo "Microsoft Edge" $edgeExe $edgeData
  if ($edge) { return $edge }

  # --- Google Chrome (fallback) ---
  $chromeExe = Find-BrowserExe `
    @("SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\chrome.exe") `
    @(
      "$env:ProgramFiles\Google\Chrome\Application\chrome.exe",
      "${env:ProgramFiles(x86)}\Google\Chrome\Application\chrome.exe",
      "$env:LOCALAPPDATA\Google\Chrome\Application\chrome.exe"
    )
  $chromeData = Join-Path $env:LOCALAPPDATA "Google\Chrome\User Data"
  return Get-BrowserInfo "Google Chrome" $chromeExe $chromeData
}

$Browser = Detect-DefaultBrowser
if ($Browser) {
  Write-Host ""
  Write-Host "Detected browser: $($Browser.Name)"
  Write-Host "  Install path:    $($Browser.ExePath) $(if ($Browser.Version) { '(' + $Browser.Version + ')' })"
  Write-Host "  User Data:       $($Browser.UserDataDir)"
  Write-Host "  Profile:         $($Browser.ProfileDir)"
  Write-Host "  Extensions dir:  $($Browser.ExtensionsDir)"
  Write-Host "  (Unpacked extensions are registered by folder path via Developer mode;"
  Write-Host "   copying into Extensions dir alone does not install them.)"
} else {
  Write-Host ""
  Write-Host "No Edge/Chrome installation detected."
}

# ---------------------------------------------------------------------------
# Download & install the extension
# ---------------------------------------------------------------------------
$TmpDir = Join-Path ([System.IO.Path]::GetTempPath()) ("cookievault-" + [Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Force -Path $TmpDir | Out-Null

try {
  Write-Host ""
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
    $msg = $_.Exception.Message
    if ($msg -match "404") {
      Write-Host "No GitHub Release published yet (404), falling back to source archive."
    } else {
      Write-Host "No release asset available ($msg), falling back to source archive."
    }
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
  if ($Browser) {
    Write-Host "  1. Opening $($Browser.Name) extensions page..."
    Write-Host "  2. Enable `"Developer mode`" (top-right toggle)."
    Write-Host "  3. Click `"Load unpacked`" and select:"
    Write-Host "       $InstallDir"
  } else {
    Write-Host "  1. Open your browser's extensions page:"
    Write-Host "       Chrome: chrome://extensions/"
    Write-Host "       Edge:   edge://extensions/"
    Write-Host "  2. Enable `"Developer mode`" (top-right toggle)."
    Write-Host "  3. Click `"Load unpacked`" and select:"
    Write-Host "       $InstallDir"
  }
  Write-Host "  4. Start the Go server (see repo README), then open the"
  Write-Host "     extension options page and pair with the server's pair code."
  Write-Host ""

  # 6) Optionally open the detected browser's extensions page
  if ($LaunchBrowser -and $Browser) {
    $page = if ($Browser.Name -eq "Microsoft Edge") { "edge://extensions/" } else { "chrome://extensions/" }
    Write-Host "Opening $page in $($Browser.Name)..."
    Start-Process -FilePath $Browser.ExePath -ArgumentList $page
  }
  Write-Host "Done."
} finally {
  Remove-Item $TmpDir -Recurse -Force -ErrorAction SilentlyContinue
}
