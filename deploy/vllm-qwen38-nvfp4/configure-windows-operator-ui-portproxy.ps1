[CmdletBinding()]
param(
  # Optional: the WSL distro's eth0 address changes on every WSL restart, so
  # a scheduled task should omit -WslAddress and pass -Distro instead to
  # have this script look the current address up each run.
  [ValidatePattern('^\d{1,3}(\.\d{1,3}){3}$')]
  [string]$WslAddress,

  [string]$Distro,

  [int]$GrafanaPort = 32030,
  [int]$LangfusePort = 32031
)

$ErrorActionPreference = 'Stop'

if (-not ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
    [Security.Principal.WindowsBuiltInRole]::Administrator)) {
  throw 'Run this script from an elevated PowerShell session on the Windows host.'
}

if (-not $WslAddress) {
  if (-not $Distro) {
    throw 'Pass either -WslAddress or -Distro (to look the current address up).'
  }
  # Parsed in PowerShell, not a shell pipeline inside `wsl.exe -- sh -lc '...'`:
  # quoting a pipe-and-awk one-liner through bash -> ssh -> PowerShell -> wsl.exe
  # -> sh in the same string is fragile (verified to silently drop the awk
  # stage in practice), whereas one plain `ip` call plus a regex is not.
  $raw = (wsl.exe -d $Distro -- ip -4 -o addr show dev eth0) -join "`n"
  if ($raw -notmatch '(\d{1,3}(?:\.\d{1,3}){3})/') {
    throw "Could not determine WSL address for distro '$Distro' (got: '$raw')."
  }
  $WslAddress = $Matches[1]
}

function Set-PortProxy([int]$Port, [string]$Name) {
  & netsh interface portproxy delete v4tov4 listenaddress=0.0.0.0 listenport=$Port | Out-Null
  & netsh interface portproxy add v4tov4 listenaddress=0.0.0.0 listenport=$Port connectaddress=$WslAddress connectport=$Port

  Get-NetFirewallRule -DisplayName $Name -ErrorAction SilentlyContinue | Remove-NetFirewallRule
  New-NetFirewallRule -DisplayName $Name -Direction Inbound -Action Allow -Protocol TCP -LocalPort $Port `
    -Profile Private -RemoteAddress LocalSubnet | Out-Null
}

Set-PortProxy -Port $GrafanaPort -Name 'LLM Stack Grafana (LAN)'
Set-PortProxy -Port $LangfusePort -Name 'LLM Stack Langfuse (LAN)'

Write-Host "Grafana:  http://grafana.gpu-host.local:$GrafanaPort"
Write-Host "Langfuse: http://langfuse.gpu-host.local:$LangfusePort"
