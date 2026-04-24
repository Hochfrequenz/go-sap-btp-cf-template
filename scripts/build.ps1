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

$env:CGO_ENABLED = "0"
$env:GOOS        = "linux"
$env:GOARCH      = "amd64"

& go build -o "bin/server" "./cmd/server"
if ($LASTEXITCODE -ne 0) {
    Write-Error "go build failed with exit code $LASTEXITCODE"
    exit $LASTEXITCODE
}

Write-Host "built bin/server"
Get-Item "bin/server" | Format-List FullName, Length, LastWriteTime
