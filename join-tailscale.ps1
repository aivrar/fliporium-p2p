# Force the Tailscale Windows client to use our Headscale server,
# then join the fliporium tailnet with the pre-auth key.
# Run from an elevated PowerShell.

$ErrorActionPreference = 'Stop'

$LoginServer = 'https://headscale.fliporium.com'
$AuthKey     = 'hskey-auth-MelyjF4P5Zvl-Aon7k9ei829mUuEvfWPYhWPJmgAFq92FVU9Oqz64uJBCMvl3fhctRQecVBgNl7Kk'
$Cli         = 'C:\Program Files\Tailscale\tailscale.exe'
$RegPath     = 'HKLM:\SOFTWARE\Tailscale IPN'

# 1. Confirm elevation.
$elev = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $elev) {
    Write-Error 'Run this from an elevated PowerShell (Win+X -> Terminal (Admin)).'
    exit 1
}

# 2. Tell Tailscale where to look BEFORE it starts dialing out.
Write-Host '[1/6] Setting registry LoginURL and UnattendedMode...'
if (-not (Test-Path $RegPath)) {
    New-Item -Path $RegPath -Force | Out-Null
}
New-ItemProperty -Path $RegPath -Name 'LoginURL'       -Value $LoginServer -PropertyType String -Force | Out-Null
New-ItemProperty -Path $RegPath -Name 'UnattendedMode' -Value 'always'     -PropertyType String -Force | Out-Null
Write-Host '   set.'

# 3. Restart Tailscale so it picks up the registry change.
Write-Host '[2/6] Restarting Tailscale service...'
Restart-Service -Name Tailscale -Force
Start-Sleep -Seconds 4

# 4. Log out any leftover pending state.
Write-Host '[3/6] tailscale logout (clearing any pending auth)...'
& $Cli logout 2>$null | Out-Null

# 5. Bring up the interface against Headscale.
Write-Host "[4/6] tailscale up --login-server=$LoginServer ..."
& $Cli up --login-server=$LoginServer --auth-key=$AuthKey --accept-routes --reset --unattended
$upExit = $LASTEXITCODE
if ($upExit -ne 0) {
    Write-Warning "tailscale up returned exit code $upExit"
}

# 6. Show what we ended up with.
Write-Host ''
Write-Host '[5/6] tailscale status:'
& $Cli status
Write-Host ''
Write-Host '[6/6] tailscale ip:'
& $Cli ip -4 2>&1
& $Cli ip -6 2>&1
Write-Host ''
Write-Host 'Done. If you see a 100.x.y.z address above, the join worked.'
