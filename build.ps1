# Build both Fliporium binaries: the Wails GUI and the headless CLI.
# Requires Go on PATH (or the user-scope install at ~/go-sdk/bin).
#
# Usage:
#   .\build.ps1            # build both
#   .\build.ps1 -Gui       # GUI only
#   .\build.ps1 -Cli       # CLI only

param(
    [switch]$Gui,
    [switch]$Cli
)

$ErrorActionPreference = 'Stop'

# Ensure go.exe is reachable; fall back to the user-scope SDK.
$env:Path = "$env:USERPROFILE\go-sdk\bin;$env:USERPROFILE\go\bin;$env:Path"
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    Write-Error "go.exe not found on PATH. Install Go or check ~/go-sdk/bin."
    exit 1
}

$buildAll = -not ($Gui -or $Cli)

if ($Gui -or $buildAll) {
    Write-Host "Building fliporium.exe (Wails GUI)..." -ForegroundColor Cyan
    # The 'desktop,production' tags + windowsgui linker flag are mandatory for the
    # Wails v2 runtime to start cleanly. Bare 'go build' pops an Error dialog.
    & go build -tags 'desktop,production' -ldflags '-H windowsgui -s -w' -o fliporium.exe ./cmd/fliporium
    if ($LASTEXITCODE -ne 0) { Write-Error "GUI build failed"; exit 1 }
    Write-Host ("  -> {0:F1} MB" -f ((Get-Item fliporium.exe).Length / 1MB)) -ForegroundColor Green
}

if ($Cli -or $buildAll) {
    Write-Host "Building fliporium-cli.exe (terminal peer)..." -ForegroundColor Cyan
    & go build -o fliporium-cli.exe ./cmd/fliporium-cli
    if ($LASTEXITCODE -ne 0) { Write-Error "CLI build failed"; exit 1 }
    Write-Host ("  -> {0:F1} MB" -f ((Get-Item fliporium-cli.exe).Length / 1MB)) -ForegroundColor Green
}

Write-Host "Done." -ForegroundColor Green
