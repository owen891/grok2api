$ErrorActionPreference = "Continue"

$mutex = [Threading.Mutex]::new($false, "Local\Grok2API-local-backend-supervisor")
if (-not $mutex.WaitOne(0)) { exit 0 }

$repo = (Resolve-Path (Join-Path $PSScriptRoot "..\..")).Path
$backend = Join-Path $repo "backend"
$exe = Join-Path $repo ".tmp\grok2api-local.exe"
$config = Join-Path $repo "config.yaml"
$logDir = Join-Path $repo ".tmp\grok2api-logs"
$compose = Join-Path $repo "docker-compose.yml"
$proxyPort = if ($env:GROK2API_PROXY_PORT) { [int]$env:GROK2API_PROXY_PORT } else { 7890 }
$proxyWaitDeadline = (Get-Date).AddSeconds(120)

New-Item -ItemType Directory -Force -Path (Split-Path $exe), $logDir | Out-Null

function Test-LocalPort([int] $Port) {
    return [bool](Get-NetTCPConnection -LocalAddress "127.0.0.1" -LocalPort $Port -State Listen -ErrorAction SilentlyContinue)
}

function Wait-ForProxy {
    if (Test-LocalPort $proxyPort) { return $true }
    if ((Get-Date) -ge $proxyWaitDeadline) { return $true }
    return $false
}

function Ensure-BrowserWorker {
    if (Test-LocalPort 8192) { return }
    try {
        & docker compose -f $compose up -d grok-web-browser *> (Join-Path $logDir "docker-compose.log")
    } catch {
        Add-Content -Path (Join-Path $logDir "supervisor.log") -Value "$(Get-Date -Format o) docker: $($_.Exception.Message)"
    }
}

while ($true) {
    Ensure-BrowserWorker

    if (-not (Wait-ForProxy)) {
        Start-Sleep -Seconds 5
        continue
    }

    if (Test-LocalPort 8000) {
        Start-Sleep -Seconds 15
        continue
    }

    $stamp = Get-Date -Format "yyyyMMdd-HHmmss"
    $stdout = Join-Path $logDir "backend-$stamp.out.log"
    $stderr = Join-Path $logDir "backend-$stamp.err.log"
    try {
        $process = Start-Process -FilePath $exe `
            -ArgumentList @("--config", $config, "--listen", "127.0.0.1:8000") `
            -WorkingDirectory $repo -WindowStyle Hidden `
            -RedirectStandardOutput $stdout -RedirectStandardError $stderr -PassThru
        Wait-Process -Id $process.Id
    } catch {
        Add-Content -Path (Join-Path $logDir "supervisor.log") -Value "$(Get-Date -Format o) backend: $($_.Exception.Message)"
    }
    Start-Sleep -Seconds 5
}
