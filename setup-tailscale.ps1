# Phase 1 verification: install Tailscale Windows client and join the fliporium tailnet.
# Run this in an **elevated** PowerShell window (right-click PowerShell -> Run as administrator).

$LoginServer = 'https://headscale.fliporium.com'
$AuthKey     = 'hskey-auth-MelyjF4P5Zvl-Aon7k9ei829mUuEvfWPYhWPJmgAFq92FVU9Oqz64uJBCMvl3fhctRQecVBgNl7Kk'

# 1. Confirm we are elevated.
$elev = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $elev) {
    Write-Error "This script must be run from an elevated PowerShell. Right-click PowerShell -> Run as administrator, then re-run."
    exit 1
}

# 2. Install Tailscale via winget if not already present.
$ts = Get-Command tailscale -ErrorAction SilentlyContinue
if (-not $ts) {
    Write-Host "Installing Tailscale via winget..."
    winget install --id Tailscale.Tailscale --accept-package-agreements --accept-source-agreements --silent --disable-interactivity
    if ($LASTEXITCODE -ne 0) {
        Write-Error "winget install failed with exit code $LASTEXITCODE. Try installing manually from https://tailscale.com/download/windows"
        exit 1
    }
    # Tailscale CLI lives at C:\Program Files\Tailscale\tailscale.exe; PATH refresh may need a new session.
    $env:Path = $env:Path + ';C:\Program Files\Tailscale'
} else {
    Write-Host "Tailscale already installed at $($ts.Source)"
}

# 3. Resolve the CLI path explicitly (in case PATH wasn't picked up in this session).
$cli = (Get-Command tailscale -ErrorAction SilentlyContinue).Source
if (-not $cli) { $cli = 'C:\Program Files\Tailscale\tailscale.exe' }
if (-not (Test-Path $cli)) {
    Write-Error "Could not find tailscale.exe after install. Expected $cli"
    exit 1
}
Write-Host "Using tailscale at: $cli"
& $cli version

# 4. Join the tailnet.
Write-Host ""
Write-Host "Joining tailnet at $LoginServer ..."
& $cli up --login-server=$LoginServer --auth-key=$AuthKey --accept-routes --reset
if ($LASTEXITCODE -ne 0) {
    Write-Error "tailscale up failed with exit code $LASTEXITCODE"
    exit 1
}

# 5. Show status.
Write-Host ""
Write-Host "--- tailscale status ---"
& $cli status

Write-Host ""
Write-Host "--- tailscale ip ---"
& $cli ip -4
& $cli ip -6

Write-Host ""
Write-Host "Done. Reply 'joined' in the chat and I'll verify from the Headscale side."
