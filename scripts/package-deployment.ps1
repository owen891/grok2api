param(
    [string]$Output = (Join-Path $PSScriptRoot '..\dist\grok2api-deployment'),
    [switch]$Offline,
    [ValidateSet('protocol', 'browser', 'both', 'none')]
    [string]$RegistrationRuntime = 'protocol'
)

$ErrorActionPreference = 'Stop'
$repo = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$outputPath = if ([System.IO.Path]::IsPathRooted($Output)) {
    [System.IO.Path]::GetFullPath($Output)
} else {
    [System.IO.Path]::GetFullPath((Join-Path (Get-Location) $Output))
}

$comparison = [System.StringComparison]::OrdinalIgnoreCase
$directorySeparator = [System.IO.Path]::DirectorySeparatorChar
$alternateSeparator = [System.IO.Path]::AltDirectorySeparatorChar
$normalizedRepo = $repo.TrimEnd($directorySeparator, $alternateSeparator)
$normalizedOutput = $outputPath.TrimEnd($directorySeparator, $alternateSeparator)
$normalizedRoot = ([System.IO.Path]::GetPathRoot($outputPath)).TrimEnd($directorySeparator, $alternateSeparator)
if (
    [string]::IsNullOrWhiteSpace($normalizedOutput) -or
    $normalizedOutput.Equals($normalizedRoot, $comparison) -or
    $normalizedRepo.Equals($normalizedOutput, $comparison) -or
    $normalizedRepo.StartsWith($normalizedOutput + $directorySeparator, $comparison)
) {
    throw "Refusing unsafe deployment output path: $outputPath"
}

if (Test-Path $outputPath) {
    Remove-Item -LiteralPath $outputPath -Recurse -Force
}
New-Item -ItemType Directory -Path $outputPath -Force | Out-Null
New-Item -ItemType Directory -Path (Join-Path $outputPath 'images') -Force | Out-Null

$files = @(
    '.env.production.example',
    'compose.production.yml',
    'compose.browser-registration.yml',
    'compose.registration.yml',
    'config.production.example.yaml',
    'install.ps1',
    'install.sh',
    'README.md',
    'DEPLOYMENT-SIMPLIFICATION.md',
    'turnstile-solver-entrypoint.sh'
)
foreach ($file in $files) {
    $source = Join-Path $repo "deploy_artifact\$file"
    if (-not (Test-Path $source)) { throw "Missing deployment input: $source" }
    Copy-Item -LiteralPath $source -Destination (Join-Path $outputPath $file)
}

Copy-Item -LiteralPath (Join-Path $repo 'scripts\grok_web_browser_worker.py') `
    -Destination (Join-Path $outputPath 'grok_web_browser_worker.py')

$productionCompose = Join-Path $outputPath 'compose.production.yml'
$composeText = Get-Content $productionCompose -Raw
$composeText = $composeText.Replace('../scripts/grok_web_browser_worker.py', './grok_web_browser_worker.py')
Set-Content -LiteralPath $productionCompose -Value $composeText -NoNewline

if ($Offline) {
    $envFile = Join-Path $repo 'deploy_artifact\.env.production.example'
    $solverImage = ((Get-Content $envFile | Where-Object { $_ -match '^GROK2API_SOLVER_IMAGE=' }) -split '=', 2)[1]
    $appImage = ((Get-Content $envFile | Where-Object { $_ -match '^GROK2API_IMAGE=' }) -split '=', 2)[1]
    $browserRegistrationImage = ((Get-Content $envFile | Where-Object { $_ -match '^GROK2API_BROWSER_IMAGE=' }) -split '=', 2)[1]
    $browserImage = 'flaresolverr:offline'
    $images = @($browserImage, 'postgres:16-alpine', 'redis:7-alpine')
    if ($RegistrationRuntime -in @('protocol', 'none')) { $images += $appImage }
    if ($RegistrationRuntime -in @('browser', 'both')) { $images += $browserRegistrationImage }
    if ($RegistrationRuntime -in @('protocol', 'both')) { $images += $solverImage }
    $images = $images | Select-Object -Unique
    docker tag 'ghcr.io/flaresolverr/flaresolverr@sha256:139dfee1c6f89249c8d665d1333a42e8ec74ec0a86bc6bb1c8461e10d3a66a47' $browserImage
    foreach ($image in $images) {
        Write-Host "Saving $image"
        $safe = ($image -replace '[^A-Za-z0-9_.-]', '_')
        docker save $image -o (Join-Path $outputPath "images\$safe.tar")
    }
    $offlineEnv = Join-Path $outputPath '.env.offline'
    $envText = Get-Content (Join-Path $outputPath '.env.production.example') -Raw
    $envText = $envText -replace '(?m)^REGISTRATION_RUNTIME=.*$', "REGISTRATION_RUNTIME=$RegistrationRuntime"
    Add-Content -LiteralPath $offlineEnv -Value ($envText + "`r`nFLARESOLVERR_IMAGE=$browserImage`r`n")
}

$zip = "$outputPath.zip"
if (Test-Path $zip) { Remove-Item -LiteralPath $zip -Force }
Compress-Archive -Path (Join-Path $outputPath '*') -DestinationPath $zip -CompressionLevel Optimal

$folderBytes = (Get-ChildItem $outputPath -Recurse -File | Measure-Object Length -Sum).Sum
$zipBytes = (Get-Item $zip).Length
Write-Host ("Folder: {0:N2} MB" -f ($folderBytes / 1MB))
Write-Host ("Zip:    {0:N2} MB" -f ($zipBytes / 1MB))
Write-Host "Output: $zip"
