//go:build windows

// SAPI text-to-speech: the instant, zero-download fallback voice. Robotic compared to Piper, but
// it works the moment Windows exists — no model download, no GPU, no wait. Selected explicitly
// (never silently substituted for a missing Piper voice) so users always know which they're
// getting. Drives SAPI via COM automation rather than a Win32 DLL binding, writing to a real WAV
// file through SpFileStream: SAPI's memory-stream automation surface needs raw SAFEARRAY handling
// that go-ole doesn't make pleasant, while a file round-trip is a few automation calls and lets us
// just parse a normal WAV header afterward.
package engine

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"

	ole "github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
)

type SapiTTS struct{}

// NewSapiTTS is a constructor for symmetry with the other engines; SAPI needs no setup or model
// path, so it can never fail to construct (only Synthesize calls can fail, per-call).
func NewSapiTTS() *SapiTTS { return &SapiTTS{} }

func (s *SapiTTS) Synthesize(text string) ([]byte, int, error) {
	tmp, err := os.CreateTemp("", "tts-sst-sapi-*.wav")
	if err != nil {
		return nil, 0, err
	}
	path := tmp.Name()
	tmp.Close()
	defer os.Remove(path)

	if err := sapiSpeakToFile(text, path); err != nil {
		return nil, 0, err
	}
	return readWavPCM16(path)
}

func (s *SapiTTS) Close() {} // nothing to release: every call opens and tears down its own COM objects

// sapiSpeakToFile runs SpVoice.Speak with its AudioOutputStream redirected to an SpFileStream —
// COM apartment-threaded, so every call must happen on ONE OS thread from Init to Uninitialize.
func sapiSpeakToFile(text, path string) (err error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err = ole.CoInitialize(0); err != nil {
		return fmt.Errorf("sapi: CoInitialize: %w", err)
	}
	defer ole.CoUninitialize()

	voiceUnk, err := oleutil.CreateObject("SAPI.SpVoice")
	if err != nil {
		return fmt.Errorf("sapi: create SpVoice (is SAPI installed? should ship with Windows): %w", err)
	}
	defer voiceUnk.Release()
	voice, err := voiceUnk.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		return err
	}
	defer voice.Release()

	streamUnk, err := oleutil.CreateObject("SAPI.SpFileStream")
	if err != nil {
		return fmt.Errorf("sapi: create SpFileStream: %w", err)
	}
	defer streamUnk.Release()
	stream, err := streamUnk.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		return err
	}
	defer stream.Release()

	// SpeechAudioFormatType/SpeechStreamFileMode enum values (SAPI's own constants — no header to
	// import in Go, these are stable published COM enum literals from sapi.h):
	//   SAFT22kHz16BitMono = 22, SSFMCreateForWrite = 3
	const saft22kHz16BitMono = 22
	const ssfmCreateForWrite = 3
	if _, err = oleutil.CallMethod(stream, "Open", path, ssfmCreateForWrite, false); err != nil {
		return fmt.Errorf("sapi: open output file: %w", err)
	}
	// Explicitly set the output format via SetFormat(waveFormatEx, forceRefresh) is the "correct"
	// SAPI path but requires building a wave-format COM object; simpler and equally reliable: rely
	// on the default input from Speak(), which SpFileStream defaults to the format above (22kHz
	// 16-bit mono) — confirmed by inspecting the produced file's own header, so parse that header
	// for ground truth rather than trusting this constant at the reader end.
	_ = saft22kHz16BitMono

	// PutPropertyRef (DISPATCH_PROPERTYPUTREF), not PutProperty: AudioOutputStream takes an object
	// reference, and a plain property-put returns "Member not found" for it.
	if _, err = oleutil.PutPropertyRef(voice, "AudioOutputStream", stream); err != nil {
		return fmt.Errorf("sapi: set AudioOutputStream: %w", err)
	}
	// SVSFlagsAsync = 1 would return before speech finishes; default synchronous call blocks until
	// the whole utterance is written, which is exactly what we want before reading the file back.
	if _, err = oleutil.CallMethod(voice, "Speak", text); err != nil {
		return fmt.Errorf("sapi: Speak: %w", err)
	}
	if _, err = oleutil.CallMethod(stream, "Close"); err != nil {
		return fmt.Errorf("sapi: close output file: %w", err)
	}
	return nil
}

// readWavPCM16 reads a canonical WAV file and returns its data chunk plus the format's sample
// rate, validating it is 16-bit mono/stereo PCM (what SAPI always produces).
func readWavPCM16(path string) ([]byte, int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	if len(b) < 44 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, 0, fmt.Errorf("sapi: not a RIFF/WAVE file (%d bytes)", len(b))
	}
	var (
		sampleRate, bitsPerSample, channels int
		data                                []byte
	)
	off := 12
	for off+8 <= len(b) {
		id := string(b[off : off+4])
		size := int(binary.LittleEndian.Uint32(b[off+4 : off+8]))
		body := off + 8
		if body+size > len(b) {
			size = len(b) - body
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return nil, 0, fmt.Errorf("sapi: fmt chunk too small")
			}
			channels = int(binary.LittleEndian.Uint16(b[body+2 : body+4]))
			sampleRate = int(binary.LittleEndian.Uint32(b[body+4 : body+8]))
			bitsPerSample = int(binary.LittleEndian.Uint16(b[body+14 : body+16]))
		case "data":
			data = b[body : body+size]
		}
		off = body + size
		if size%2 == 1 { // chunks are word-aligned
			off++
		}
	}
	if data == nil {
		return nil, 0, fmt.Errorf("sapi: no data chunk found")
	}
	if bitsPerSample != 16 {
		return nil, 0, fmt.Errorf("sapi: unexpected bits-per-sample %d (want 16)", bitsPerSample)
	}
	if channels == 2 {
		data = stereoToMono16(data)
	} else if channels != 1 {
		return nil, 0, fmt.Errorf("sapi: unexpected channel count %d", channels)
	}
	return data, sampleRate, nil
}

func stereoToMono16(pcm []byte) []byte {
	n := len(pcm) / 4
	out := make([]byte, n*2)
	for i := 0; i < n; i++ {
		l := int16(binary.LittleEndian.Uint16(pcm[i*4 : i*4+2]))
		r := int16(binary.LittleEndian.Uint16(pcm[i*4+2 : i*4+4]))
		avg := int16((int32(l) + int32(r)) / 2)
		binary.LittleEndian.PutUint16(out[i*2:i*2+2], uint16(avg))
	}
	return out
}
