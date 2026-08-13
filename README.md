# tts-sst

Local speech-to-text and text-to-speech for Windows, served over the
[Wyoming protocol](https://github.com/rhasspy/wyoming) on localhost — no Docker, no Python,
no cloud. Built for [open-quake](https://github.com/TeeJS/open-quake)'s Claude Voice panel,
usable by any Wyoming client (Home Assistant included).

- **STT**: Whisper or Parakeet models via [sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx) — port 10300
- **TTS**: Piper voices via sherpa-onnx — port 10200
- Binds 127.0.0.1 only by default.

## Status

Early development. Working today: Wyoming server, Piper-voice TTS, mock engines for protocol
testing. Coming: STT models, first-run model download, tray app, installer.

## Dev

Requires Go 1.22+ and a MinGW-w64 gcc on PATH (cgo). Build:

```
go build -o build/tts-sst.exe ./cmd/tts-sst
```

Copy `onnxruntime.dll`, `sherpa-onnx-c-api.dll`, `sherpa-onnx-cxx-api.dll` from
`$(go env GOMODCACHE)/github.com/k2-fsa/sherpa-onnx-go-windows@*/lib/x86_64-pc-windows-gnu/`
next to the exe.

Models live in `%APPDATA%\tts-sst\models\` — e.g. extract
[vits-piper-en_US-lessac-medium](https://github.com/k2-fsa/sherpa-onnx/releases/tag/tts-models)
there and run `tts-sst.exe`. `-mock` serves tone/echo engines with no models for protocol tests.
