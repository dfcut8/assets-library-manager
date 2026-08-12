[CmdletBinding()]
param(
    [Parameter()]
    [string] $InstallDir = 'D:\assets-library'
)

$ErrorActionPreference = 'Stop'

$repositoryRoot = Split-Path -Parent $PSScriptRoot
$buildDirectory = Join-Path $repositoryRoot 'bin'
$builtBinary = Join-Path $buildDirectory 'asset-library-manager-windows-amd64.exe'
$exampleConfig = Join-Path $repositoryRoot 'config.example.json'

New-Item -ItemType Directory -Force -Path $buildDirectory | Out-Null

Push-Location $repositoryRoot
try {
    $env:CGO_ENABLED = '0'
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    & go build -trimpath -o $builtBinary ./cmd/asset-library-manager
    if ($LASTEXITCODE -ne 0) {
        throw "go build failed with exit code $LASTEXITCODE"
    }
}
finally {
    Pop-Location
}

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
Copy-Item -Force -LiteralPath $builtBinary -Destination (Join-Path $InstallDir 'asset-library-manager.exe')
Copy-Item -Force -LiteralPath $exampleConfig -Destination (Join-Path $InstallDir 'config.example.json')

$installedConfig = Join-Path $InstallDir 'config.json'
if (-not (Test-Path -LiteralPath $installedConfig)) {
    Copy-Item -LiteralPath $exampleConfig -Destination $installedConfig
}
