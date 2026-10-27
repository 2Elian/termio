param(
    [string]$Version = "0.1.0"
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root
New-Item -ItemType Directory -Force -Path dist | Out-Null

$ldflags = "-s -w -X termio/internal/version.Version=$Version"
$targets = @(
    @{ OS = "windows"; Arch = "amd64"; Ext = ".exe" },
    @{ OS = "windows"; Arch = "arm64"; Ext = ".exe" },
    @{ OS = "darwin"; Arch = "amd64"; Ext = "" },
    @{ OS = "darwin"; Arch = "arm64"; Ext = "" },
    @{ OS = "linux"; Arch = "amd64"; Ext = "" },
    @{ OS = "linux"; Arch = "arm64"; Ext = "" }
)

$env:CGO_ENABLED = "0"
foreach ($target in $targets) {
    $env:GOOS = $target.OS
    $env:GOARCH = $target.Arch
    $ext = $target.Ext
    $name = "$($target.OS)-$($target.Arch)"
    Write-Host "building termio $name"
    go build -trimpath -ldflags $ldflags -o "dist/termio-$name$ext" ./cmd/termio
    if ($LASTEXITCODE -ne 0) { throw "termio build failed for $name" }
    Write-Host "building gateway $name"
    go build -trimpath -ldflags $ldflags -o "dist/termio-gateway-$name$ext" ./cmd/termio-gateway
    if ($LASTEXITCODE -ne 0) { throw "gateway build failed for $name" }
    Write-Host "building termioctl $name"
    go build -trimpath -ldflags $ldflags -o "dist/termioctl-$name$ext" ./cmd/termioctl
    if ($LASTEXITCODE -ne 0) { throw "termioctl build failed for $name" }
}

foreach ($arch in @("amd64", "arm64")) {
    $env:GOOS = "windows"
    $env:GOARCH = $arch
    Write-Host "building Windows GUI agent windows-$arch"
    go build -trimpath -ldflags "$ldflags -H=windowsgui" -o "dist/termio-agent-windows-$arch.exe" ./cmd/termio
    if ($LASTEXITCODE -ne 0) { throw "GUI agent build failed for windows-$arch" }
}

Remove-Item Env:CGO_ENABLED, Env:GOOS, Env:GOARCH -ErrorAction SilentlyContinue
Write-Host "Done. Artifacts are in dist/"
