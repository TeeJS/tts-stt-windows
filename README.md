# tts-sst

Local speech-to-text and text-to-speech for Windows, served over the
[Wyoming protocol](https://github.com/rhasspy/wyoming) on localhost — no Docker, no Python, no
cloud, no account. Built for [open-quake](https://github.com/TeeJS/open-quake)'s Claude Voice
panel; works with any Wyoming client, Home Assistant included.

- **Speech-to-text** on port **10300** — Whisper (via [sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx))
- **Text-to-speech** on port **10200** — Piper voices, or the built-in Windows voice
- Binds `127.0.0.1` only by default; ~11 MB download, models fetched on first run
- Lives in the system tray: status, model/voice pickers, start-with-Windows

## Install

Download the latest `tts-sst-*-win-x64.zip`, unzip anywhere, run `tts-sst.exe`. A tray icon
appears and the first run downloads the default models (a ~64 MB Piper voice, then a ~600 MB
Whisper model) — the status line shows progress. Right-click the tray icon to switch models,
pick the instant Windows built-in voice, or enable start-with-Windows.

Clients connect to `127.0.0.1:10300` (STT) and `127.0.0.1:10200` (TTS). In open-quake's Claude
Voice app those are the shipped defaults.

## Voices and models

| Pick | Notes |
|---|---|
| Piper (default) | Natural-sounding neural voice, ~64 MB, runs on CPU |
| Windows built-in | The old SAPI voice. Robotic, but instant — no download at all |
| Whisper small.en (default) | Best accuracy, ~600 MB |
| Whisper base.en | Smaller and faster, less accurate |

Everything runs on the CPU; no GPU required.

## Settings

`%APPDATA%\tts-sst\config.json` — bind address, ports, chosen model/voice, language, thread count.
Models live in `%APPDATA%\tts-sst\models\`, the log in `%APPDATA%\tts-sst\tts-sst.log`.

Flags override config for one run: `-console` (no tray), `-mock` (protocol testing, no models),
`-no-download`, `-bind`, `-stt-port`, `-tts-port`, `-threads`, `-models`.

## Build

Needs Go 1.22+ and a MinGW-w64 gcc on PATH (cgo — build time only).

```powershell
.\build.ps1
```

Produces `dist\` (exe + three sherpa-onnx DLLs) and a release zip. `go test ./...` covers the
protocol framing, the model installer, and real SAPI synthesis; the engine tests need the DLLs on
PATH (`$env:Path = "$PWD\dist;$env:Path"`).

## License

MIT. Bundles [sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx) (Apache-2.0) and downloads
Piper/Whisper models published under their own licenses.
