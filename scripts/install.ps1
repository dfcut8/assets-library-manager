[CmdletBinding()]
param(
    [Parameter()]
    [string] $InstallDir = 'D:\assets-library'
)

$ErrorActionPreference = 'Stop'

$repositoryRoot = Split-Path -Parent $PSScriptRoot
$buildDirectory = Join-Path $repositoryRoot 'bin'
$builtBinary = Join-Path $buildDirectory 'asset-library-manager-windows-amd64.exe'

New-Item -ItemType Directory -Force -Path $buildDirectory | Out-Null

Push-Location $repositoryRoot
try {
    $version = (& git describe --tags --always --dirty).Trim()
    if ($LASTEXITCODE -ne 0) {
        throw "git describe failed with exit code $LASTEXITCODE"
    }
    $commit = (& git rev-parse HEAD).Trim()
    if ($LASTEXITCODE -ne 0) {
        throw "git rev-parse failed with exit code $LASTEXITCODE"
    }

    $env:CGO_ENABLED = '0'
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    $linkerFlags = "-s -w -X main.version=$version -X main.commit=$commit"
    & go build -trimpath -ldflags $linkerFlags -o $builtBinary ./cmd/asset-library-manager
    if ($LASTEXITCODE -ne 0) {
        throw "go build failed with exit code $LASTEXITCODE"
    }
}
finally {
    Pop-Location
}

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
Copy-Item -Force -LiteralPath $builtBinary -Destination (Join-Path $InstallDir 'asset-library-manager.exe')
