$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot

docker version | Out-Null
docker compose version | Out-Null

if (-not (Test-Path .env)) { Copy-Item .env.production.example .env }
if (-not (Test-Path config.production.yaml)) { Copy-Item config.production.example.yaml config.production.yaml }
New-Item -ItemType Directory -Force data | Out-Null

$configText = Get-Content .env, config.production.yaml -Raw
if ($configText -match 'replace-with|change-me') {
    throw 'Edit .env and config.production.yaml first: replace all secret placeholders. POSTGRES_PASSWORD must match the PostgreSQL DSN password.'
}

$runtime = $env:REGISTRATION_RUNTIME
if ([string]::IsNullOrWhiteSpace($runtime)) {
    $runtimeLine = Get-Content .env | Where-Object { $_ -match '^REGISTRATION_RUNTIME=' } | Select-Object -Last 1
    $runtime = if ($runtimeLine) { ($runtimeLine -split '=', 2)[1].Trim() } else { 'protocol' }
}
$runtime = $runtime.ToLowerInvariant()
$composeArgs = @('--env-file', '.env', '-f', 'compose.production.yml')
switch ($runtime) {
    'protocol' { $composeArgs += @('-f', 'compose.registration.yml') }
    'browser' { $composeArgs += @('-f', 'compose.browser-registration.yml') }
    'both' {
        $composeArgs += @('-f', 'compose.registration.yml', '-f', 'compose.browser-registration.yml')
    }
    'none' { }
    default { throw 'REGISTRATION_RUNTIME must be protocol, browser, both, or none.' }
}

Write-Host "Registration runtime: $runtime"
docker compose @composeArgs config --quiet
docker compose @composeArgs pull
docker compose @composeArgs up -d

$port = 8000
$envFile = Get-Content .env | Where-Object { $_ -match '^GROK2API_PORT=' }
if ($envFile) { $port = ($envFile -split '=', 2)[1] }
for ($i = 0; $i -lt 30; $i++) {
    try {
        $response = Invoke-WebRequest "http://127.0.0.1:$port/healthz" -UseBasicParsing -TimeoutSec 2
        if ($response.StatusCode -eq 200) { Write-Host 'grok2api is healthy'; exit 0 }
    } catch { Start-Sleep -Seconds 2 }
}
Write-Error "grok2api did not become healthy. Inspect: docker compose $($composeArgs -join ' ') logs --tail=100 grok2api"
exit 1
