# Package extension\ into cookievault-extension.zip for a GitHub Release.
param([string]$Out = "cookievault-extension.zip")
$ErrorActionPreference = "Stop"
$Root = Split-Path (Split-Path $MyInvocation.MyCommand.Path -Parent) -Parent
Set-Location $Root
if (Test-Path $Out) { Remove-Item $Out -Force }
Compress-Archive -Path "extension\*" -DestinationPath $Out -Force
Get-Item $Out | Format-Table Name, Length
Write-Host "Attach $Out to a GitHub Release, e.g.:"
Write-Host "  gh release create v0.1.0 $Out --title 'CookieVault v0.1.0'"
