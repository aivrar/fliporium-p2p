# Launch the Fliporium GUI.
#
# Identity and chat history live in the data dir; each data dir is its own
# independent install with a separate Ed25519 identity, generated on first
# launch. No auth key or signup -- paste an invite link (or create a room)
# from inside the app.
#
# Usage:
#   .\run.ps1                  # default data dir (fliporium-data next to the exe)
#   .\run.ps1 -Name alice      # data dir fliporium-data-alice -- handy for
#                              #   running a second independent instance locally
#   .\run.ps1 -DataDir D:\foo  # explicit data/identity folder

param(
    [string]$Name,
    [string]$DataDir
)

$ErrorActionPreference = 'Stop'

if (-not $DataDir -and $Name) {
    $DataDir = Join-Path $PSScriptRoot ("fliporium-data-" + $Name)
}
if ($DataDir) { $env:FLIPORIUM_DIR = $DataDir }

$exe = Join-Path $PSScriptRoot 'fliporium.exe'
if (-not (Test-Path $exe)) {
    Write-Host "fliporium.exe not built; running build.ps1..." -ForegroundColor Cyan
    & (Join-Path $PSScriptRoot 'build.ps1') -Gui
}

$dirMsg = if ($DataDir) { $DataDir } else { 'fliporium-data (default)' }
Write-Host "Launching fliporium.exe -- dir=$dirMsg" -ForegroundColor Cyan

# GUI runs detached so this window returns to the prompt.
Start-Process -FilePath $exe
