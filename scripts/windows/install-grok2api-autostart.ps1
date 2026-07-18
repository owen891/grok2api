$ErrorActionPreference = "Stop"

$repo = (Resolve-Path (Join-Path $PSScriptRoot "..\..")).Path
$backend = Join-Path $repo "backend"
$exe = Join-Path $repo ".tmp\grok2api-local.exe"
$supervisor = Join-Path $repo "scripts\windows\grok2api-supervisor.ps1"
$config = Join-Path $repo "config.yaml"

if (-not (Test-Path $config)) {
    throw "Missing $config"
}

New-Item -ItemType Directory -Force -Path (Split-Path $exe) | Out-Null
Push-Location $backend
try {
    & go build -trimpath -o $exe ./cmd/grok2api
    if ($LASTEXITCODE -ne 0) { throw "go build failed with exit code $LASTEXITCODE" }
} finally {
    Pop-Location
}

$taskName = "Grok2API local backend"
$powershell = (Get-Command powershell.exe).Source
$arguments = "-NoLogo -NoProfile -NonInteractive -WindowStyle Hidden -ExecutionPolicy Bypass -File `"$supervisor`""
$action = New-ScheduledTaskAction -Execute $powershell -Argument $arguments -WorkingDirectory $repo
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
$settings = New-ScheduledTaskSettingsSet -StartWhenAvailable -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit ([TimeSpan]::Zero)
$principal = New-ScheduledTaskPrincipal -UserId $env:USERNAME -LogonType Interactive -RunLevel Limited

Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $trigger -Settings $settings -Principal $principal -Force | Out-Null
Start-ScheduledTask -TaskName $taskName
Write-Host "Installed '$taskName' for $env:USERNAME"
Write-Host "Backend: http://127.0.0.1:8000/healthz"
Write-Host "Logs: $repo\.tmp\grok2api-logs"
