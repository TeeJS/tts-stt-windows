# Builds tts-sst.exe with its sherpa-onnx DLLs into dist\ and zips them for release.
# Requires Go and a MinGW-w64 gcc on PATH (cgo). Run from the repo root:  .\build.ps1
param(
    [switch]$Sign,                            # Authenticode-sign the exe (see docs/signing.md)
    [string]$Version = "0.1.0"
)
$ErrorActionPreference = "Stop"

$dist = Join-Path $PSScriptRoot "dist"
Remove-Item $dist -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Path $dist | Out-Null

# -H windowsgui: no console window when launched from Explorer or at login. The tray icon is the UI.
Write-Host "Building tts-sst.exe..."
$env:CGO_ENABLED = "1"
& go build -ldflags "-H windowsgui -s -w" -o (Join-Path $dist "tts-sst.exe") ./cmd/tts-sst
if ($LASTEXITCODE -ne 0) { throw "go build failed" }

# The sherpa-onnx runtime DLLs live in the Go module cache; copy the x86_64 set next to the exe.
$modCache = (& go env GOMODCACHE).Trim()
$libDir = Get-ChildItem -Path (Join-Path $modCache "github.com\k2-fsa\sherpa-onnx-go-windows@*") -Directory |
    Sort-Object Name | Select-Object -Last 1 |
    ForEach-Object { Join-Path $_.FullName "lib\x86_64-pc-windows-gnu" }
if (-not (Test-Path $libDir)) { throw "sherpa-onnx DLLs not found — run 'go mod download' first" }
Copy-Item (Join-Path $libDir "*.dll") $dist
Write-Host "Copied runtime DLLs from $libDir"

if ($Sign) {
    Write-Host "Signing..."
    & signtool sign /v /debug /fd SHA256 /tr "http://timestamp.acs.microsoft.com" /td SHA256 `
        /dlib "$env:LOCALAPPDATA\Microsoft\Azure.CodeSigning.Dlib\Azure.CodeSigning.Dlib.dll" `
        /dmdf (Join-Path $PSScriptRoot "signing-metadata.json") (Join-Path $dist "tts-sst.exe")
    if ($LASTEXITCODE -ne 0) { throw "signing failed" }
}

$zip = Join-Path $PSScriptRoot "tts-sst-$Version-win-x64.zip"
Remove-Item $zip -Force -ErrorAction SilentlyContinue
Compress-Archive -Path (Join-Path $dist "*") -DestinationPath $zip
Write-Host "`nBuilt $zip"
Get-ChildItem $dist | Format-Table Name, @{n = "MB"; e = { [math]::Round($_.Length / 1MB, 1) } }
