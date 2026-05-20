# Bypass the broken router DNS by pinning headscale.fliporium.com in hosts,
# then retry the tailnet join.
# Run from an elevated PowerShell.

Start-Transcript -Path 'E:\fliporium\.fix-log.txt' -Force | Out-Null
$ErrorActionPreference = 'Stop'

$LoginServer = 'https://headscale.fliporium.com'
$AuthKey     = 'hskey-auth-MelyjF4P5Zvl-Aon7k9ei829mUuEvfWPYhWPJmgAFq92FVU9Oqz64uJBCMvl3fhctRQecVBgNl7Kk'
$Cli         = 'C:\Program Files\Tailscale\tailscale.exe'
$HostsFile   = 'C:\Windows\System32\drivers\etc\hosts'
$TargetIP    = '160.153.177.57'
$TargetHost  = 'headscale.fliporium.com'

$elev = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $elev) { Write-Error 'Run elevated.'; exit 1 }

Write-Host '[1/6] Ensuring hosts entry...'
$hostsContent = Get-Content $HostsFile -Raw
if ($hostsContent -notmatch [regex]::Escape($TargetHost)) {
    Add-Content -Path $HostsFile -Value "`n$TargetIP $TargetHost  # fliporium phase 1"
    Write-Host '   added.'
} else {
    Write-Host '   already present.'
}

Write-Host '[2/6] Flushing DNS cache...'
ipconfig /flushdns | Out-Null

Write-Host '[3/6] Verifying name resolves correctly now...'
$r = Test-NetConnection -ComputerName $TargetHost -Port 443 -WarningAction SilentlyContinue
Write-Host ("   resolved={0} reachable={1} ip={2}" -f $r.NameResolutionSucceeded, $r.TcpTestSucceeded, $r.RemoteAddress)
if (-not $r.NameResolutionSucceeded) { Write-Error 'name still does not resolve after hosts edit'; exit 1 }

Write-Host '[4/6] Restarting Tailscale to clear cached state...'
Restart-Service Tailscale -Force
Start-Sleep -Seconds 4

Write-Host '[5/6] tailscale up against Headscale...'
& $Cli logout 2>$null | Out-Null
& $Cli up --login-server=$LoginServer --auth-key=$AuthKey --accept-routes --reset --unattended
$upExit = $LASTEXITCODE
Write-Host ("   up exit code: $upExit")

Write-Host ''
Write-Host '[6/6] tailscale status:'
& $Cli status
Write-Host ''
Write-Host 'tailscale ip:'
& $Cli ip -4 2>&1
& $Cli ip -6 2>&1

Stop-Transcript | Out-Null
