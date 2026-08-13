# Builds tts-sst.exe with its sherpa-onnx DLLs into dist\ and zips them for release.
# Requires Go and a MinGW-w64 gcc on PATH (cgo). Run from the repo root:  .\build.ps1
param(
    [switch]$Sign,                            # Authenticode-sign the exe (see docs/signing.md)
    [string]$Version = "0.1.0"
)
$ErrorActionPreference = "Stop"

$dist = Join-Path $PSScriptRoot "dist"
# Clear the CONTENTS rather than the directory: a terminal sitting in dist\ (or a previous
# tts-sst.exe still running) locks the directory itself, and that shouldn't fail a build.
New-Item -ItemType Directory -Path $dist -Force | Out-Null
try { Get-ChildItem $dist -Force | Remove-Item -Recurse -Force -ErrorAction Stop }
catch { throw "Could not clear $dist — is tts-sst.exe still running? ($($_.Exception.Message))" }

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
    # Authenticode via Azure Trusted Signing. The signing material is NOT in this repo: fetch the
    # dlib into .signing\ per docs/signing.md, and sign in with Connect-AzAccount first. The token
    # comes from that session through per-user PowerShell 7 — which must be on PATH, because the
    # dlib unwraps it with a PS7-only parameter and silently falls back to a browser otherwise.
    $signing = Join-Path $PSScriptRoot ".signing"
    if (-not (Test-Path "$signing\dlib-x64\Azure.CodeSigning.Dlib.dll")) {
        throw "Signing requested but .signing\dlib-x64 is missing — see docs/signing.md"
    }
    $signtool = Get-ChildItem "C:\Program Files (x86)\Windows Kits\10\bin" -Recurse -Filter signtool.exe -ErrorAction SilentlyContinue |
        Where-Object { $_.Directory.Name -eq 'x64' } | Sort-Object FullName -Descending | Select-Object -First 1 -ExpandProperty FullName
    if (-not $signtool) { throw "signtool.exe not found — install the Windows 10/11 SDK" }

    $env:AZURE_TENANT_ID = "a4ae2122-9515-4f85-abc8-71c29ccc261f"   # pin the tenant; a personal MS account otherwise resolves to the wrong one
    if (Test-Path "$env:LOCALAPPDATA\pwsh7\pwsh.exe") { $env:PATH = "$env:LOCALAPPDATA\pwsh7;$env:PATH" }

    Write-Host "Signing tts-sst.exe..."
    & $signtool sign /v /fd SHA256 /tr "http://timestamp.acs.microsoft.com" /td SHA256 `
        /dlib "$signing\dlib-x64\Azure.CodeSigning.Dlib.dll" /dmdf "$signing\metadata.json" `
        (Join-Path $dist "tts-sst.exe")
    if ($LASTEXITCODE -ne 0) { throw "signing failed" }

    & $signtool verify /pa /v (Join-Path $dist "tts-sst.exe")
    if ($LASTEXITCODE -ne 0) { throw "signature verification failed" }
}

# Deliberately version-free: GitHub serves the newest release's assets at
# /releases/latest/download/<name>, so a stable filename gives open-quake (and anyone else) a
# permanent download link. The version lives in the release tag, not the filename.
$zip = Join-Path $PSScriptRoot "tts-sst-win-x64.zip"
Remove-Item $zip -Force -ErrorAction SilentlyContinue
Compress-Archive -Path (Join-Path $dist "*") -DestinationPath $zip
Write-Host "`nBuilt $zip  (version $Version)"
Get-ChildItem $dist | Format-Table Name, @{n = "MB"; e = { [math]::Round($_.Length / 1MB, 1) } }
