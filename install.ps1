# install.ps1 — one-shot installer for the Claude Usage Dashboard tray app.
#
# Run from an interactive PowerShell session (no admin required for
# per-user Task Scheduler entries):
#
#     powershell -ExecutionPolicy Bypass -File .\install.ps1
#
# The script:
#   1. Locates trayapp.exe (next to the script unless -ExePath overrides).
#   2. Bootstraps a default prices.yaml in %APPDATA%\usage_dashboard if
#      one isn't already present, copied from config\prices.example.yaml.
#   3. Registers a per-user Task Scheduler "at logon" task so the tray
#      starts automatically (preferred over shell:startup; survives RDP).
#
# Re-running is safe: existing config is left untouched, and the
# scheduled task is replaced in-place.
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

# --- 1. Bootstrap default config ---------------------------------------------

$AppDir       = Join-Path $env:APPDATA       'usage_dashboard'
$LocalAppDir  = Join-Path $env:LOCALAPPDATA  'usage_dashboard'
$ConfigPath   = Join-Path $AppDir 'prices.yaml'
$ExampleSrc   = Join-Path $PSScriptRoot 'config\prices.example.yaml'

foreach ($d in @($AppDir, $LocalAppDir)) {
    if (-not (Test-Path $d)) {
        New-Item -ItemType Directory -Path $d -Force | Out-Null
        Write-Host "Created $d"
    }
}

if (Test-Path $ConfigPath) {
    Write-Host "prices.yaml already present at $ConfigPath; leaving untouched."
} elseif (Test-Path $ExampleSrc) {
    Copy-Item -Path $ExampleSrc -Destination $ConfigPath
    Write-Host "Bootstrapped default prices.yaml -> $ConfigPath"
} else {
    Write-Warning "config\prices.example.yaml not found at $ExampleSrc; skipping config bootstrap."
}

# --- 2. Register Task Scheduler "at logon" task ------------------------------

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
