package diarize

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"
)

// buildWAV assembles a RIFF/WAVE file from raw sample bytes.
func buildWAV(format uint16, channels, rate, bits int, payload []byte, extensible bool) []byte {
	var fmtChunk bytes.Buffer
	fmtTag := format
	if extensible {
		fmtTag = 0xFFFE
	}
	binary.Write(&fmtChunk, binary.LittleEndian, fmtTag)
	binary.Write(&fmtChunk, binary.LittleEndian, uint16(channels))
	binary.Write(&fmtChunk, binary.LittleEndian, uint32(rate))
	binary.Write(&fmtChunk, binary.LittleEndian, uint32(rate*channels*bits/8))
	binary.Write(&fmtChunk, binary.LittleEndian, uint16(channels*bits/8))
	binary.Write(&fmtChunk, binary.LittleEndian, uint16(bits))
	if extensible {
		binary.Write(&fmtChunk, binary.LittleEndian, uint16(22)) // cbSize
		binary.Write(&fmtChunk, binary.LittleEndian, uint16(bits))
		binary.Write(&fmtChunk, binary.LittleEndian, uint32(0x3)) // channel mask
		binary.Write(&fmtChunk, binary.LittleEndian, format)      // SubFormat leads with the code
		fmtChunk.Write([]byte{0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71})
	}

	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(4+8+fmtChunk.Len()+8+len(payload)))
	b.WriteString("WAVE")
	b.WriteString("fmt ")
	binary.Write(&b, binary.LittleEndian, uint32(fmtChunk.Len()))
	b.Write(fmtChunk.Bytes())
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(len(payload)))
	b.Write(payload)
	return b.Bytes()
}

func s16le(vals ...int16) []byte {
	var b bytes.Buffer
	for _, v := range vals {
		binary.Write(&b, binary.LittleEndian, v)
	}
	return b.Bytes()
}

func TestDecodeWAV16BitMonoPassthrough(t *testing.T) {
	wav := buildWAV(1, 1, 16000, 16, s16le(0, 16384, -16384, 32767), false)
	got, err := DecodeAudio(wav)
	if err != nil {
		t.Fatal(err)
	}
	want := []float32{0, 0.5, -0.5, 32767.0 / 32768}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if math.Abs(float64(got[i]-want[i])) > 1e-6 {
			t.Errorf("got[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestDecodeWAVStereoDownmix(t *testing.T) {
	// L=0.5, R=-0.5 -> 0; L=R=0.25 -> 0.25
	wav := buildWAV(1, 2, 16000, 16, s16le(16384, -16384, 8192, 8192), false)
	got, err := DecodeAudio(wav)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || math.Abs(float64(got[0])) > 1e-6 || math.Abs(float64(got[1]-0.25)) > 1e-6 {
		t.Fatalf("downmix = %v", got)
	}
}

func TestDecodeWAVFloat32Extensible(t *testing.T) {
	var payload bytes.Buffer
	for _, v := range []float32{0.25, -0.75} {
		binary.Write(&payload, binary.LittleEndian, v)
	}
	wav := buildWAV(3, 1, 16000, 32, payload.Bytes(), true)
	got, err := DecodeAudio(wav)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != 0.25 || got[1] != -0.75 {
		t.Fatalf("got %v", got)
	}
}

func TestDecodeWAV24Bit(t *testing.T) {
	// +2^22 = 0.5, -2^22 = -0.5 in 24-bit
	payload := []byte{0x00, 0x00, 0x40, 0x00, 0x00, 0xC0}
	wav := buildWAV(1, 1, 16000, 24, payload, false)
	got, err := DecodeAudio(wav)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || math.Abs(float64(got[0]-0.5)) > 1e-6 || math.Abs(float64(got[1]+0.5)) > 1e-6 {
		t.Fatalf("got %v", got)
	}
}

func TestDecodeWAVResamplesTo16k(t *testing.T) {
	// 48 kHz mono, 4800 samples of a 440 Hz tone -> expect 1600 samples out.
	n := 4800
	var payload bytes.Buffer
	for i := 0; i < n; i++ {
		v := int16(10000 * math.Sin(2*math.Pi*440*float64(i)/48000))
		binary.Write(&payload, binary.LittleEndian, v)
	}
	wav := buildWAV(1, 1, 48000, 16, payload.Bytes(), false)
	got, err := DecodeAudio(wav)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1600 {
		t.Fatalf("len = %d, want 1600", len(got))
	}
}

func TestDecodeRejectsUnknownContainer(t *testing.T) {
	var uerr *UnsupportedAudioError
	_, err := DecodeAudio([]byte("OggS this is not audio we support"))
	if !errors.As(err, &uerr) {
		t.Fatalf("err = %v, want UnsupportedAudioError", err)
	}
}

func TestDecodeRoutesMP3(t *testing.T) {
	// Real MP3 decoding is covered by integration tests with actual recordings;
	// here we only prove the routing recognizes MP3-ish payloads and reports
	// decode failures as UnsupportedAudioError.
	var uerr *UnsupportedAudioError
	// Bare ID3 tag with garbage after it.
	if _, err := DecodeAudio(append([]byte("ID3"), bytes.Repeat([]byte{0}, 64)...)); !errors.As(err, &uerr) {
		t.Errorf("ID3 route: err = %v, want UnsupportedAudioError", err)
	}
	// RIFF wrapper whose fmt tag is 0x0055 (MPEG Layer 3) with a garbage payload.
	wav := buildWAV(0x0055, 2, 44100, 0, bytes.Repeat([]byte{0xAB}, 64), false)
	if _, err := DecodeAudio(wav); !errors.As(err, &uerr) {
		t.Errorf("MP3-in-RIFF route: err = %v, want UnsupportedAudioError", err)
	}
}

// snr computes the signal-to-noise ratio in dB of got vs a reference tone.
func snr(got []float32, freq float64, rate, from, to int) float64 {
	var sig, noise float64
	for n := from; n < to; n++ {
		want := math.Sin(2 * math.Pi * freq * float64(n) / float64(rate))
		sig += want * want
		d := float64(got[n]) - want
		noise += d * d
	}
	return 10 * math.Log10(sig/noise)
}

// TestResampleLongFileSNR is the regression test for the float32-position bug the
// Python service once had: resampling with float32 source coordinates is exact only
// to 2^24 samples, so audio past ~6 minutes at 48 kHz degraded (20.4 dB -> 6.3 dB
// SNR). 400 s at 48 kHz crosses that boundary; SNR at the end must match the start.
func TestResampleLongFileSNR(t *testing.T) {
	if testing.Short() {
		t.Skip("long-file resample test")
	}
	const (
		inRate  = 48000
		outRate = 16000
		seconds = 400
		freq    = 1000.0
	)
	in := make([]float32, inRate*seconds)
	for i := range in {
		in[i] = float32(math.Sin(2 * math.Pi * freq * float64(i) / inRate))
	}
	out := Resample(in, inRate, outRate)
	if len(out) != outRate*seconds {
		t.Fatalf("len = %d, want %d", len(out), outRate*seconds)
	}
	early := snr(out, freq, outRate, 1*outRate, 5*outRate)
	late := snr(out, freq, outRate, (seconds-5)*outRate, (seconds-1)*outRate)
	t.Logf("SNR early %.1f dB, late %.1f dB", early, late)
	if early < 40 {
		t.Errorf("early SNR %.1f dB too low — resampler quality broken", early)
	}
	if late < early-3 {
		t.Errorf("late SNR %.1f dB degraded vs early %.1f dB — position-precision regression", late, early)
	}
}

func TestResampleUpsample(t *testing.T) {
	const inRate, outRate, freq = 8000, 16000, 440.0
	in := make([]float32, inRate*2)
	for i := range in {
		in[i] = float32(math.Sin(2 * math.Pi * freq * float64(i) / inRate))
	}
	out := Resample(in, inRate, outRate)
	if len(out) != outRate*2 {
		t.Fatalf("len = %d, want %d", len(out), outRate*2)
	}
	if s := snr(out, freq, outRate, outRate/2, outRate*3/2); s < 40 {
		t.Errorf("upsample SNR %.1f dB too low", s)
	}
}
