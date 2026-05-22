# Build the Fliporium binaries: the Wails GUI (the product) and the signaling
# server. Requires Go on PATH (or the user-scope install at ~/go-sdk/bin).
#
# Usage:
#   .\build.ps1            # build both
#   .\build.ps1 -Gui       # GUI only
#   .\build.ps1 -Signal    # signaling server only

param(
    [switch]$Gui,
    [switch]$Signal
)

$ErrorActionPreference = 'Stop'

# Ensure go.exe is reachable; fall back to the user-scope SDK.
$env:Path = "$env:USERPROFILE\go-sdk\bin;$env:USERPROFILE\go\bin;$env:Path"
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    Write-Error "go.exe not found on PATH. Install Go or check ~/go-sdk/bin."
    exit 1
}

$buildAll = -not ($Gui -or $Signal)

if ($Gui -or $buildAll) {
    Write-Host "Building fliporium.exe (Wails GUI)..." -ForegroundColor Cyan
    # The 'desktop,production' tags + windowsgui linker flag are mandatory for the
    # Wails v2 runtime to start cleanly. Bare 'go build' pops an Error dialog.
    & go build -tags 'desktop,production' -ldflags '-H windowsgui -s -w' -o fliporium.exe ./cmd/fliporium
    if ($LASTEXITCODE -ne 0) { Write-Error "GUI build failed"; exit 1 }
    Write-Host ("  -> {0:F1} MB" -f ((Get-Item fliporium.exe).Length / 1MB)) -ForegroundColor Green
}

if ($Signal -or $buildAll) {
    Write-Host "Building flipsignal.exe (signaling server)..." -ForegroundColor Cyan
    & go build -o flipsignal.exe ./cmd/flipsignal
    if ($LASTEXITCODE -ne 0) { Write-Error "signaling build failed"; exit 1 }
    Write-Host ("  -> {0:F1} MB" -f ((Get-Item flipsignal.exe).Length / 1MB)) -ForegroundColor Green
}

Write-Host "Done." -ForegroundColor Green
