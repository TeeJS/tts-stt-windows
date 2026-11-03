package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/TeeJS/tts-stt-windows/internal/localvoices"
)

// A raw Piper export (no sherpa metadata, piper1-gpl phoneme map with diphthongs) must load
// and speak after localvoices.Prepare. Needs the sherpa DLLs beside the test binary (see
// pocketModelDir), an installed catalog Piper voice to borrow espeak-ng-data from, and the
// trained-voice fixture kept outside the repo:
//
//	go test -c -o dist\engine.test.exe .\internal\engine; cd dist; .\engine.test.exe --% -test.run TestLocalVoice -test.v
func TestLocalVoiceEndToEnd(t *testing.T) {
	exe, _ := os.Executable()
	if !isFile(filepath.Join(filepath.Dir(exe), "onnxruntime.dll")) {
		t.Skip("run the compiled test binary from dist\\ (see pocketModelDir)")
	}
	fixture := filepath.Join(`D:\Github\_backups\norm-voice\piper-raw\norm`, "norm")
	if !isFile(fixture+".onnx") || !isFile(fixture+".onnx.json") {
		t.Skip("trained-voice fixture not present")
	}
	espeak := filepath.Join(os.Getenv("APPDATA"), "tts-sst", "models", "vits-piper-en_GB-cori-medium-int8", "espeak-ng-data")
	if !isDir(espeak) {
		t.Skip("no installed catalog Piper voice to borrow espeak-ng-data from")
	}

	// Copy the fixture into a temp root under its own name; the backup stays pristine.
	root := t.TempDir()
	dir := filepath.Join(root, "norm")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, ext := range []string{".onnx", ".onnx.json"} {
		b, err := os.ReadFile(fixture + ext)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "norm"+ext), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	voiceDir, err := localvoices.Prepare(root, "norm", espeak)
	if err != nil {
		t.Fatal(err)
	}
	tts, err := NewPiperTTS(voiceDir, 2)
	if err != nil {
		t.Fatalf("NewPiperTTS: %v", err)
	}
	defer tts.Close()
	pcm, rate, err := tts.Synthesize("I'd like to try my own voice today.")
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	t.Logf("synthesized %.2fs at %d Hz", float64(len(pcm)/2)/float64(rate), rate)
	if rate != 22050 || len(pcm) < 22050 {
		t.Errorf("got %d bytes at %d Hz; want ≥0.5 s at 22050 Hz", len(pcm), rate)
	}
}
