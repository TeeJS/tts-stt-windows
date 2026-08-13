# tts-sst

Local speech-to-text and text-to-speech for Windows, served over the
[Wyoming protocol](https://github.com/rhasspy/wyoming) on localhost — no Docker, no Python, no
cloud, no account. Works with any Wyoming client, Home Assistant included.

- **Speech-to-text** on port **10300** — Whisper, Parakeet, SenseVoice, Moonshine or Dolphin
- **Text-to-speech** on port **10200** — 200+ Piper and Coqui voices, or the built-in Windows voice
- **53 languages**
- Binds `127.0.0.1` only by default; ~11 MB download, models fetched on demand
- Lives in the system tray; models are chosen in a settings page

## Install

Download the latest `tts-sst-*-win-x64.zip`, unzip anywhere, run `tts-sst.exe`. A tray icon
appears and the settings page opens to ask which language you speak — pre-selected from your
Windows display language — then downloads a voice and a speech model that fit. Nothing is
downloaded before you answer.

Clients connect to `127.0.0.1:10300` (STT) and `127.0.0.1:10200` (TTS).

## Voices and models

Open **Settings & models** from the tray to browse everything, filtered by language and searchable:

| | What's available |
|---|---|
| Voices | 178 Piper + 25 Coqui voices across 47 locales, in extra-low to high quality. Multiple accents per language where they exist (16 for en-GB alone). |
| Windows built-in | The old SAPI voice: robotic, but instant and needs no download. |
| Speech recognition | Whisper (99 languages, tiny → large), Parakeet v3 (25 European languages, fast — the default), Dolphin (40 Asian languages), SenseVoice (zh/en/ja/ko/yue), Moonshine (fastest, per-language). |

Everything runs on the CPU; no GPU required. Models download on selection and can be deleted from
the same page.

The catalog is generated from the [sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx) releases and
embedded in the binary, so browsing works offline — only downloading needs the network. Refresh it
with `go run ./tools/gen-catalog`.

## Settings

The settings page (tray → **Settings & models**) covers language, speaking rate, the non-speech
filter, and a voice test. It writes `%APPDATA%\tts-sst\config.json`, which also holds the bind
address, ports and thread count. Models live in `%APPDATA%\tts-sst\models\`, the log in
`%APPDATA%\tts-sst\tts-sst.log`.

**Ignore non-speech sounds** (on by default): speech models write `(clicking)`, `[BLANK_AUDIO]` or
music markers when they hear noise rather than words, and some invent stock phrases during
silence. Those are discarded instead of being passed on as if you had said them.

Flags override config for one run: `-console` (no tray), `-mock` (protocol testing, no models),
`-no-download`, `-bind`, `-stt-port`, `-tts-port`, `-threads`, `-models`.

## Status

Working and tested end-to-end: both Wyoming services, the model browser, first-run setup, the
Windows fallback voice, and the non-speech filter. Not done yet: code signing, an installer,
winget publication.

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
