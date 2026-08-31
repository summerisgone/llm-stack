[CmdletBinding()]
param(
  [Parameter(Mandatory = $true)]
  [string]$Distro
)

# The netsh portproxy entries in configure-windows-operator-ui-portproxy.ps1
# target a fixed WSL NAT address that changes on every WSL restart, so a
# one-time manual run goes stale the next time the host or WSL restarts.
# This registers a scheduled task that re-resolves the address and reapplies
# the portproxy/firewall rules automatically: at boot, at logon, and every 5
# minutes thereafter (cheap and idempotent) so it also self-heals if WSL
# restarts on its own without a full Windows reboot.

$ErrorActionPreference = 'Stop'

if (-not ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
    [Security.Principal.WindowsBuiltInRole]::Administrator)) {
  throw 'Run this script from an elevated PowerShell session on the Windows host.'
}

$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$configureScript = Join-Path $scriptDir 'configure-windows-operator-ui-portproxy.ps1'
if (-not (Test-Path $configureScript)) {
  throw "Not found: $configureScript"
}

$taskName = 'LLM Stack Operator UI Portproxy'
$action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument `
  "-NoProfile -ExecutionPolicy Bypass -File `"$configureScript`" -Distro `"$Distro`""

$triggers = @(
  (New-ScheduledTaskTrigger -AtStartup)
  (New-ScheduledTaskTrigger -AtLogOn)
  (New-ScheduledTaskTrigger -Once -At (Get-Date) -RepetitionInterval (New-TimeSpan -Minutes 5) -RepetitionDuration (New-TimeSpan -Days 3650))
)

# S4U: runs elevated whether or not the account is logged in, without
# storing a password. Requires the account to already be a local admin.
# $env:USERDOMAIN can be wrong under some remote/service contexts (e.g. an
# SSH session reporting WORKGROUP instead of the actual machine/domain), so
# resolve the account name from the current token instead.
$currentUser = [Security.Principal.WindowsIdentity]::GetCurrent().Name
$principal = New-ScheduledTaskPrincipal -UserId $currentUser -LogonType S4U -RunLevel Highest

$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
  -StartWhenAvailable -ExecutionTimeLimit (New-TimeSpan -Minutes 2)

Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $triggers `
  -Principal $principal -Settings $settings -Force | Out-Null

Start-ScheduledTask -TaskName $taskName

Write-Host "Registered scheduled task '$taskName' (startup, logon, every 5 min) for distro '$Distro'."
