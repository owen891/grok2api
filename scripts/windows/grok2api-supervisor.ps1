$ErrorActionPreference = "Continue"

$mutex = [Threading.Mutex]::new($false, "Local\Grok2API-local-backend-supervisor")
if (-not $mutex.WaitOne(0)) { exit 0 }

$repo = (Resolve-Path (Join-Path $PSScriptRoot "..\..")).Path
$exe = Join-Path $repo ".tmp\grok2api-local.exe"
$config = Join-Path $repo "config.yaml"
$logDir = Join-Path $repo ".tmp\grok2api-logs"
$compose = Join-Path $repo "docker-compose.yml"
$proxyPort = if ($env:GROK2API_PROXY_PORT) { [int]$env:GROK2API_PROXY_PORT } else { 7890 }
$proxyURL = if ($env:GROK2API_WEB_PROXY_URL) { $env:GROK2API_WEB_PROXY_URL } else { "http://127.0.0.1:$proxyPort" }
$proxyWaitDeadline = (Get-Date).AddSeconds(120)
$lastEgressCheck = [datetime]::MinValue

New-Item -ItemType Directory -Force -Path (Split-Path $exe), $logDir | Out-Null

function Write-SupervisorLog([string] $Message) {
    Add-Content -Path (Join-Path $logDir "supervisor.log") -Value "$(Get-Date -Format o) $Message"
}

function Get-PortFromEnvironment([string] $Name, [int] $Default) {
    $raw = [Environment]::GetEnvironmentVariable($Name)
    if ([string]::IsNullOrWhiteSpace($raw)) { return $Default }
    $value = 0
    if (-not [int]::TryParse($raw, [ref]$value) -or $value -lt 1 -or $value -gt 65535) {
        throw "$Name must be a TCP port between 1 and 65535"
    }
    return $value
}

function Test-LocalPort([int] $Port) {
    return [bool](Get-NetTCPConnection -LocalAddress "127.0.0.1" -LocalPort $Port -State Listen -ErrorAction SilentlyContinue)
}

function Get-ExcludedTcpRanges {
    $lines = & netsh interface ipv4 show excludedportrange protocol=tcp 2>$null
    foreach ($line in $lines) {
        if ($line -match '^\s*(\d+)\s+(\d+)\s*$') {
            [pscustomobject]@{ Start = [int]$Matches[1]; End = [int]$Matches[2] }
        }
    }
}

function Test-PortExcluded([int] $Port) {
    foreach ($range in @(Get-ExcludedTcpRanges)) {
        if ($Port -ge $range.Start -and $Port -le $range.End) { return $true }
    }
    return $false
}

function Test-WorkerHealth([int] $Port) {
    try {
        $response = Invoke-WebRequest -UseBasicParsing -TimeoutSec 3 -Uri "http://127.0.0.1:$Port/healthz"
        if ($response.StatusCode -ne 200) { return $false }
        $body = $response.Content | ConvertFrom-Json
        return [bool]$body.ok
    } catch {
        return $false
    }
}

function Select-WorkerPort {
    $raw = [Environment]::GetEnvironmentVariable("GROK_WEB_BROWSER_WORKER_PORT")
    if (-not [string]::IsNullOrWhiteSpace($raw)) {
        $explicit = Get-PortFromEnvironment "GROK_WEB_BROWSER_WORKER_PORT" 18292
        if (Test-PortExcluded $explicit) {
            throw "GROK_WEB_BROWSER_WORKER_PORT=$explicit is in a Windows excluded TCP range"
        }
        return $explicit
    }

    # 18292 is the documented default; scan forward so a Windows/Hyper-V
    # reservation or another local service cannot strand the supervisor.
    foreach ($candidate in 18292..18392) {
        if (Test-PortExcluded $candidate) { continue }
        if (-not (Test-LocalPort $candidate) -or (Test-WorkerHealth $candidate)) {
            return $candidate
        }
    }
    throw "no usable host port found for grok-web-browser in 18292..18392"
}

$workerPort = Select-WorkerPort
$workerURL = "http://127.0.0.1:$workerPort"
# Compose uses the port variable for the host mapping. The local backend
# reads the URL override, so config.yaml and Compose cannot drift apart.
$env:GROK_WEB_BROWSER_WORKER_PORT = "$workerPort"
$env:GROK_WEB_BROWSER_WORKER_URL = $workerURL

function Ensure-BrowserWorker {
    if (Test-WorkerHealth $workerPort) { return $true }
    try {
        & docker compose -f $compose up -d --force-recreate grok-web-browser *> (Join-Path $logDir "docker-compose.log")
        if ($LASTEXITCODE -ne 0) {
            Write-SupervisorLog "browser worker compose failed with exit code $LASTEXITCODE"
            return $false
        }
    } catch {
        Write-SupervisorLog "browser worker compose failed: $($_.Exception.Message)"
        return $false
    }
    for ($attempt = 0; $attempt -lt 30; $attempt++) {
        if (Test-WorkerHealth $workerPort) { return $true }
        Start-Sleep -Seconds 2
    }
    Write-SupervisorLog "browser worker did not pass $workerURL/healthz"
    return $false
}

function Wait-ForProxy {
    if (Test-ProxyAvailable) { return $true }
    if ((Get-Date) -ge $proxyWaitDeadline) {
        Write-SupervisorLog "proxy $proxyURL is not listening; continuing without automatic Web egress provisioning"
        return $true
    }
    return $false
}

function Test-ProxyAvailable {
    try {
        $uri = [Uri]$proxyURL
        if ($uri.Host -notin @("127.0.0.1", "localhost", "::1")) { return $true }
        $port = $uri.Port
        if ($port -le 0) { $port = if ($uri.Scheme -eq "https") { 443 } else { 80 } }
        return Test-LocalPort $port
    } catch {
        Write-SupervisorLog "invalid GROK2API_WEB_PROXY_URL; automatic Web egress provisioning is disabled"
        return $false
    }
}

function Get-BootstrapPassword {
    if (-not [string]::IsNullOrWhiteSpace($env:GROK2API_ADMIN_PASSWORD)) {
        return $env:GROK2API_ADMIN_PASSWORD
    }
    if (-not (Test-Path -LiteralPath $config)) { return "" }
    $insideBootstrap = $false
    foreach ($line in (Get-Content -LiteralPath $config)) {
        if ($line -match '^\s*bootstrapAdmin:\s*$') {
            $insideBootstrap = $true
            continue
        }
        if ($insideBootstrap -and $line -match '^\S') { break }
        if ($insideBootstrap -and $line -match '^\s+password:\s*(.+?)\s*$') {
            $value = $Matches[1].Trim()
            if (($value.StartsWith('"') -and $value.EndsWith('"')) -or ($value.StartsWith("'") -and $value.EndsWith("'"))) {
                $value = $value.Substring(1, $value.Length - 2)
            }
            return $value
        }
    }
    return ""
}

function Get-AdminToken {
    $password = Get-BootstrapPassword
    if ([string]::IsNullOrWhiteSpace($password)) { return "" }
    try {
        $body = @{ username = "admin"; password = $password } | ConvertTo-Json -Compress
        $response = Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:8000/api/admin/v1/auth/login" -ContentType "application/json" -Body $body -TimeoutSec 10
        return [string]$response.data.tokens.accessToken
    } catch {
        Write-SupervisorLog "admin API login failed while checking Web egress"
        return ""
    }
}

function Ensure-WebEgressNode {
    if ($env:GROK2API_AUTO_CONFIGURE_WEB_EGRESS -eq "0") { return }
    if (-not (Test-ProxyAvailable)) { return }
    $token = Get-AdminToken
    if ([string]::IsNullOrWhiteSpace($token)) {
        Write-SupervisorLog "Web egress check skipped; set GROK2API_ADMIN_PASSWORD if bootstrapAdmin.password is not valid"
        return
    }
    $headers = @{ Authorization = "Bearer $token" }
    try {
        $response = Invoke-RestMethod -Method Get -Uri "http://127.0.0.1:8000/api/admin/v1/egress-nodes?scope=grok_web" -Headers $headers -TimeoutSec 10
        $items = @($response.data.items)
        if ($items.Count -gt 0) { return }
        $payload = @{
            name = "Local Web Proxy"
            scope = "grok_web"
            enabled = $true
            proxyURL = $proxyURL
            userAgent = ""
        } | ConvertTo-Json -Compress
        Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:8000/api/admin/v1/egress-nodes" -Headers $headers -ContentType "application/json" -Body $payload -TimeoutSec 10 | Out-Null
        Write-SupervisorLog "created default grok_web egress node for $proxyURL"
    } catch {
        Write-SupervisorLog "Web egress auto-provision failed; configure a grok_web node in the admin UI"
    }
}

while ($true) {
    if (-not (Ensure-BrowserWorker)) {
        Start-Sleep -Seconds 10
        continue
    }

    if (-not (Wait-ForProxy)) {
        Start-Sleep -Seconds 5
        continue
    }

    if (Test-LocalPort 8000) {
        if (((Get-Date) - $lastEgressCheck).TotalSeconds -ge 30) {
            Ensure-WebEgressNode
            $lastEgressCheck = Get-Date
        }
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
        Write-SupervisorLog "backend failed: $($_.Exception.Message)"
    }
    Start-Sleep -Seconds 5
}
