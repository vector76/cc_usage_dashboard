# install.ps1 — one-shot installer for the Claude Usage Dashboard tray app.
#
# Run from an interactive PowerShell session (no admin required for
# per-user Task Scheduler entries):
#
#     powershell -ExecutionPolicy Bypass -File .\install.ps1
#
# The script:
#   1. Locates trayapp.exe (next to the script unless -ExePath overrides).
#   2. Registers a per-user Task Scheduler "at logon" task so the tray
#      starts automatically (preferred over shell:startup; survives RDP).
#
# The price table no longer needs bootstrapping: the canonical prices.yaml
# is embedded in the binary (see prices_embed.go), so cost computation works
# with zero configuration. To override rates without a rebuild, drop a
# prices.yaml next to trayapp.exe or in %APPDATA%\usage_dashboard\ — see
# docs/configuration.md "Price table resolution".
#
# Re-running is safe: the scheduled task is replaced in-place.
#
# This script is documented in docs/tray-app.md ("Autostart") but not
# executed in CI — it's a Windows-only post-build step.

[CmdletBinding()]
param(
    [string]$ExePath  = (Join-Path $PSScriptRoot 'trayapp.exe'),
    [string]$TaskName = 'ClaudeUsageDashboard'
)

$ErrorActionPreference = 'Stop'

if (-not (Test-Path $ExePath)) {
    throw "trayapp.exe not found at '$ExePath'. Build it first with: go build -ldflags=`"-H=windowsgui`" -o trayapp.exe ./cmd/trayapp"
}

# --- Register Task Scheduler "at logon" task ---------------------------------

$fullUser = if ($env:USERDOMAIN) { "$env:USERDOMAIN\$env:USERNAME" } else { $env:USERNAME }

$action    = New-ScheduledTaskAction -Execute $ExePath
$trigger   = New-ScheduledTaskTrigger -AtLogOn -User $fullUser
$principal = New-ScheduledTaskPrincipal -UserId $fullUser -LogonType Interactive -RunLevel Limited
$settings  = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries `
    -DontStopIfGoingOnBatteries `
    -StartWhenAvailable `
    -ExecutionTimeLimit (New-TimeSpan -Hours 0)

if (Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue) {
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false
    Write-Host "Removed existing scheduled task '$TaskName'."
}

Register-ScheduledTask `
    -TaskName $TaskName `
    -Action $action `
    -Trigger $trigger `
    -Principal $principal `
    -Settings $settings `
    -Description 'Claude Usage Dashboard tray app (auto-start at logon).' | Out-Null

Write-Host "Registered scheduled task '$TaskName' to launch '$ExePath' at logon."
Write-Host "Done. The tray app will start automatically on next logon, or run it now with:"
Write-Host "    Start-ScheduledTask -TaskName $TaskName"
