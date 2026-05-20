# Launch the Fliporium GUI with sensible defaults.
#
# The first launch needs a Headscale pre-auth key (stored at .preauth-test for
# this dev session). Subsequent launches reuse the saved tailnet identity in
# the data dir, so no auth key is needed again.
#
# Usage:
#   .\run.ps1                          # launch GUI as hostname 'fliporium' with data in .\fliporium-data
#   .\run.ps1 -Hostname my-node        # custom tailnet hostname
#   .\run.ps1 -DataDir D:\my-data      # custom identity/data folder
#   .\run.ps1 -Cli                     # run the CLI peer instead (REPL in this window)

param(
    [string]$Hostname,
    [string]$DataDir,
    [switch]$Cli
)

$ErrorActionPreference = 'Stop'

if (-not $Hostname) { $Hostname = if ($Cli) { 'fliporium-cli' } else { 'fliporium' } }
if (-not $DataDir) { $DataDir = Join-Path $PSScriptRoot ("fliporium-data-" + $Hostname) }

$env:FLIPORIUM_HOSTNAME = $Hostname
$env:FLIPORIUM_DIR = $DataDir

# Provide an auth key only on first run (when no tailscaled.state exists yet).
$stateFile = Join-Path $DataDir 'tailscaled.state'
if (-not (Test-Path $stateFile)) {
    $preauth = Join-Path $PSScriptRoot '.preauth-test'
    if (Test-Path $preauth) {
        $line = (Get-Content $preauth | Where-Object { $_ -match '^PREAUTH_KEY=' } | Select-Object -First 1)
        if ($line) {
            $env:FLIPORIUM_AUTHKEY = ($line -replace '^PREAUTH_KEY=', '')
            Write-Host "First-run auth key loaded from .preauth-test" -ForegroundColor DarkGray
        } else {
            Write-Warning "First run but no PREAUTH_KEY= found in .preauth-test"
        }
    }
}

$exeName = if ($Cli) { 'fliporium-cli.exe' } else { 'fliporium.exe' }
$exe = Join-Path $PSScriptRoot $exeName

if (-not (Test-Path $exe)) {
    Write-Host "$exeName not built; running build.ps1..." -ForegroundColor Cyan
    & (Join-Path $PSScriptRoot 'build.ps1') $(if ($Cli) { '-Cli' } else { '-Gui' })
}

$launchMsg = "Launching ${exeName} -- hostname=${Hostname} dir=${DataDir}"
Write-Host $launchMsg -ForegroundColor Cyan

if ($Cli) {
    # CLI runs in this window so the REPL is usable.
    & $exe
} else {
    # GUI runs detached.
    Start-Process -FilePath $exe
}
