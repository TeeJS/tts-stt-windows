# Signing releases

Released binaries are Authenticode-signed with **Azure Trusted Signing** (publisher: Thomas
Schmitz). Signing is optional for development — `.\build.ps1` without `-Sign` produces an unsigned
build that runs the same, it just shows the "unknown publisher" SmartScreen prompt.

Nothing secret lives in this repo. The private key never touches the machine: SignTool sends a
digest to Azure and gets a signature back, authenticated by your own `Connect-AzAccount` session.

## One-time setup on a new machine

1. **Windows SDK** (for `signtool.exe`) and the **.NET 8 runtime** (the signing dlib needs it):
   `winget install Microsoft.DotNet.Runtime.8`
2. **Per-user PowerShell 7** — required, not optional. The dlib unwraps the Azure token with a
   PowerShell 7-only parameter; under Windows PowerShell 5.1 it silently falls back to an
   interactive browser login and the build stalls.
   ```powershell
   $dir = "$env:LOCALAPPDATA\pwsh7"
   Invoke-WebRequest -UseBasicParsing "https://github.com/PowerShell/PowerShell/releases/download/v7.4.6/PowerShell-7.4.6-win-x64.zip" -OutFile "$env:TEMP\pwsh.zip"
   Expand-Archive "$env:TEMP\pwsh.zip" $dir -Force
   & "$dir\pwsh.exe" -NoProfile -Command "Install-Module Az.Accounts -Scope CurrentUser -Force -AllowClobber"
   ```
3. **Signing client** into `.signing\` (git-ignored, re-fetch per machine):
   ```powershell
   New-Item -ItemType Directory -Force .signing\dlib-x64 | Out-Null
   Invoke-WebRequest -UseBasicParsing "https://www.nuget.org/api/v2/package/Microsoft.Trusted.Signing.Client/1.0.95" -OutFile "$env:TEMP\tsc.zip"
   Expand-Archive "$env:TEMP\tsc.zip" "$env:TEMP\tsc_x" -Force
   $d = Get-ChildItem "$env:TEMP\tsc_x" -Recurse -Filter Azure.CodeSigning.Dlib.dll | Where-Object { $_.Directory.Name -eq 'x64' } | Select-Object -First 1
   Copy-Item "$(Split-Path $d.FullName)\*" .signing\dlib-x64\ -Recurse -Force
   ```
4. **`.signing\metadata.json`** — must be **UTF-8 without a BOM**; the dlib rejects a BOM with
   `'0xEF' is an invalid start of a value`. Write it with `[IO.File]::WriteAllText`, never
   `Set-Content -Encoding utf8`:
   ```json
   {
     "Endpoint": "https://eus.codesigning.azure.net/",
     "CodeSigningAccountName": "openquakesigning",
     "CertificateProfileName": "openquake-public",
     "ExcludeCredentials": [ "EnvironmentCredential", "WorkloadIdentityCredential", "ManagedIdentityCredential", "SharedTokenCacheCredential", "VisualStudioCredential", "VisualStudioCodeCredential", "AzureCliCredential", "AzureDeveloperCliCredential", "InteractiveBrowserCredential" ]
   }
   ```
   `ExcludeCredentials` exists so a stale session fails loudly instead of opening a browser
   mid-build.

## Releasing

```powershell
Connect-AzAccount -UseDeviceAuthentication -Subscription cead6a29-d285-4141-a4e3-b249afbe1944
.\build.ps1 -Sign -Version 0.1.1
gh release create v0.1.1 tts-sst-win-x64.zip --title "tts-sst 0.1.1" --notes "..."
```

The session token lasts hours; re-run `Connect-AzAccount` when signing fails with an auth error.

**The zip filename deliberately carries no version.** GitHub serves the newest release's assets at
`/releases/latest/download/<name>`, so keeping the name fixed gives open-quake — and anyone
else — a permanent download link:

```
https://github.com/TeeJS/tts-stt-windows/releases/latest/download/tts-sst-win-x64.zip
```

Verify a build with `signtool verify /pa /v dist\tts-sst.exe`, or:
`(Get-AuthenticodeSignature dist\tts-sst.exe).Status` → `Valid`, signer `CN=Thomas Schmitz`.

This is OV, not EV: it removes "Unknown Publisher", but SmartScreen reputation still accrues with
download volume, so early downloaders may still see a warning.
