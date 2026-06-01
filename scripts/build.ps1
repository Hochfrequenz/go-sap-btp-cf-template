# scripts/build.ps1 — Windows PowerShell equivalent of `make build-linux`.
#
# Cross-compiles a static Linux/amd64 binary at ./bin/server that the
# BTP binary_buildpack can launch directly. Run this from the repo root
# before `cf push`:
#
#   .\scripts\build.ps1
#   cf push
#
# CGO_ENABLED=0 avoids needing a C toolchain; GOOS/GOARCH pin the target
# regardless of what the laptop runs on.

$ErrorActionPreference = "Stop"

New-Item -ItemType Directory -Force -Path "bin" | Out-Null

$version = & git describe --tags --always 2>$null
if ($LASTEXITCODE -ne 0 -or -not $version) { $version = "dev" }

$commit = & git rev-parse --short HEAD 2>$null
if ($LASTEXITCODE -ne 0 -or -not $commit) { $commit = "unknown" }

$branch = & git rev-parse --abbrev-ref HEAD 2>$null
if ($LASTEXITCODE -ne 0 -or -not $branch) { $branch = "unknown" }

$buildDate = [DateTime]::UtcNow.ToString("yyyy-MM-ddTHH:mm:ssZ")

$ldflags = "-X main.version=${version} -X main.commit=${commit} -X main.branch=${branch} -X main.buildDate=${buildDate}"

$env:CGO_ENABLED = "0"
$env:GOOS        = "linux"
$env:GOARCH      = "amd64"

& go build -ldflags $ldflags -o "bin/server" "./cmd/server"
if ($LASTEXITCODE -ne 0) {
    Write-Error "go build failed with exit code $LASTEXITCODE"
    exit $LASTEXITCODE
}

Write-Host "built bin/server"
Get-Item "bin/server" | Format-List FullName, Length, LastWriteTime
