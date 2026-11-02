package engine

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// pocketModelDir finds the installed PocketTTS model, or skips. Two things a clean checkout
// lacks: the ~100 MB model (downloaded through the app), and the sherpa DLLs next to the test
// binary. PATH is not enough for the latter — Windows ships an old onnxruntime.dll in System32,
// which wins over PATH and crashes sherpa — so the binary must run from dist\:
//
//	go test -c -o dist\engine.test.exe .\internal\engine; cd dist; .\engine.test.exe --% -test.run TestPocket -test.v
func pocketModelDir(t *testing.T) string {
	t.Helper()
	exe, _ := os.Executable()
	if !isFile(filepath.Join(filepath.Dir(exe), "onnxruntime.dll")) {
		t.Skip("run the compiled test binary from dist\\ (see pocketModelDir)")
	}
	base := os.Getenv("APPDATA")
	if base == "" {
		t.Skip("APPDATA not set")
	}
	matches, _ := filepath.Glob(filepath.Join(base, "tts-sst", "models", "sherpa-onnx-pocket-tts-*"))
	if len(matches) == 0 {
		t.Skip("PocketTTS model not installed")
	}
	return matches[len(matches)-1]
}

// referenceClip loads the first `secs` seconds of a sample voice the model archive ships with.
// A real voice matters: the model has no end-of-speech for a non-speech clip (a synthetic tone
// as reference produced 40 s of output for one sentence), so a tone would test the wrong thing.
func referenceClip(t *testing.T, modelDir string, secs int) ([]float32, int) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(modelDir, "test_wavs", "bria.wav"))
	if err != nil {
		t.Skip("sample clip missing: ", err)
	}
	// Walk RIFF chunks: the sample has a LIST chunk before data.
	var rate int
	for pos := 12; pos+8 <= len(data); {
		id := string(data[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(data[pos+4:]))
		body := pos + 8
		switch id {
		case "fmt ":
			if binary.LittleEndian.Uint16(data[body+2:]) != 1 || binary.LittleEndian.Uint16(data[body+14:]) != 16 {
				t.Fatal("bria.wav is not 16-bit mono PCM")
			}
			rate = int(binary.LittleEndian.Uint32(data[body+4:]))
		case "data":
			n := min(size/2, secs*rate)
			out := make([]float32, n)
			for i := range out {
				out[i] = float32(int16(binary.LittleEndian.Uint16(data[body+i*2:]))) / 32768
			}
			return out, rate
		}
		pos = body + size + size%2
	}
	t.Fatal("bria.wav: no data chunk")
	return nil, 0
}

func TestPocketTTSNeedsReference(t *testing.T) {
	dir := pocketModelDir(t)
	p, err := NewPocketTTS(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, _, err := p.Synthesize("hello"); err == nil {
		t.Error("Synthesize without a reference clip should fail")
	}
}

func TestPocketTTSSynthesize(t *testing.T) {
	dir := pocketModelDir(t)
	p, err := NewPocketTTS(dir, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	ref, rate := referenceClip(t, dir, 8)
	p.SetReference(ref, rate)
	p.SetSpeed(1.0)

	pcm, sr, err := p.Synthesize("This is a test of the cloned voice.")
	if err != nil {
		t.Fatal(err)
	}
	if sr != 24000 {
		t.Errorf("sample rate = %d, want 24000", sr)
	}
	secs := float64(len(pcm)/2) / float64(sr)
	if secs < 1 || secs > 6 {
		t.Errorf("got %.1fs of audio for a short sentence", secs)
	}
	// Close must be nil-safe like the other engines (callers close the previous engine blindly).
	var nilEngine *PocketTTS
	nilEngine.Close()
}

// A reference that isn't a voice gives the model nothing to stop on; the cap must end the
// decode early (not just trim afterwards), so this finishes in seconds, not the ~20 s it took
// uncapped, and returns no more than the limit.
func TestPocketTTSRunawayCap(t *testing.T) {
	dir := pocketModelDir(t)
	p, err := NewPocketTTS(dir, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	const rate = 16000
	tone := make([]float32, 5*rate)
	for i := range tone {
		tone[i] = 0.3 * float32(math.Sin(2*math.Pi*160*float64(i)/rate))
	}
	p.SetReference(tone, rate)
	text := "This is a test of the cloned voice."
	limit := 3 + 0.15*float64(len(text))

	t0 := time.Now()
	pcm, sr, err := p.Synthesize(text)
	if err != nil {
		t.Fatal(err)
	}
	secs := float64(len(pcm)/2) / float64(sr)
	if secs > limit+0.5 {
		t.Errorf("got %.1fs of audio, cap is %.1fs", secs, limit)
	}
	if took := time.Since(t0); took > 12*time.Second {
		t.Errorf("decode ran %v — the cap did not stop generation early", took)
	}
}
