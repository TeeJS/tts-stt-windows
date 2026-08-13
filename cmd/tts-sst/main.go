// tts-sst: local Wyoming-protocol STT/TTS server for Windows.
// v0 is a console app (tray shell comes later): loads sherpa-onnx models and serves
// Wyoming ASR on --stt-port and Wyoming TTS on --tts-port, loopback-only by default.
package main

import (
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"path/filepath"

	"github.com/TeeJS/tts-stt-windows/internal/engine"
	"github.com/TeeJS/tts-stt-windows/internal/wyoming"
)

func main() {
	var (
		bind     = flag.String("bind", "127.0.0.1", "address to bind (loopback only by default; do not expose without understanding the risk)")
		sttPort  = flag.Int("stt-port", 10300, "Wyoming ASR (speech-to-text) port")
		ttsPort  = flag.Int("tts-port", 10200, "Wyoming TTS (text-to-speech) port")
		models   = flag.String("models", defaultModelsDir(), "models directory")
		sttModel = flag.String("stt-model", "", "STT model directory name under --models (auto-detects sherpa-onnx-* when empty)")
		ttsVoice = flag.String("tts-voice", "", "TTS voice directory name under --models (auto-detects vits-piper-* when empty)")
		language = flag.String("language", "en", "STT language hint (whisper models)")
		threads  = flag.Int("threads", 4, "inference threads per engine")
		mock     = flag.Bool("mock", false, "serve mock engines (no models needed) for protocol testing")
	)
	flag.Parse()
	log.SetFlags(log.Ltime)

	var stt wyoming.STTFunc
	var tts wyoming.TTSFunc
	sttName, ttsName := "none", "none"

	if *mock {
		stt, tts = mockSTT, mockTTS
		sttName, ttsName = "mock", "mock"
	} else {
		if dir := resolveModelDir(*models, *sttModel, []string{"sherpa-onnx-*", "*whisper*", "*parakeet*"}); dir != "" {
			rec, err := loadSTT(dir, *language, *threads)
			if err != nil {
				log.Fatalf("STT model load failed: %v", err)
			}
			stt = func(pcm []byte, f wyoming.AudioFormat, _ string) (string, error) {
				return rec.Transcribe(pcm, f.Rate)
			}
			sttName = filepath.Base(dir)
		}
		if dir := resolveModelDir(*models, *ttsVoice, []string{"vits-piper-*", "vits-*"}); dir != "" {
			voice, err := engine.NewPiperTTS(dir, *threads)
			if err != nil {
				log.Fatalf("TTS voice load failed: %v", err)
			}
			tts = func(text string) ([]byte, wyoming.AudioFormat, error) {
				pcm, rate, err := voice.Synthesize(text)
				return pcm, wyoming.AudioFormat{Rate: rate, Width: 2, Channels: 1}, err
			}
			ttsName = filepath.Base(dir)
		}
		if stt == nil && tts == nil {
			log.Fatalf("no models found under %s (and --mock not set).\n"+
				"Expected: a vits-piper-* voice directory and/or a sherpa-onnx-* STT model directory.", *models)
		}
	}

	info := wyoming.BuildInfo(sttName, ttsName)
	if stt != nil {
		l := listen(*bind, *sttPort)
		log.Printf("STT (%s) listening on %s", sttName, l.Addr())
		go wyoming.ServeSTT(l, stt, info, log.Printf)
	} else {
		log.Printf("STT disabled: no model")
	}
	if tts != nil {
		l := listen(*bind, *ttsPort)
		log.Printf("TTS (%s) listening on %s", ttsName, l.Addr())
		go wyoming.ServeTTS(l, tts, info, log.Printf)
	} else {
		log.Printf("TTS disabled: no voice")
	}
	select {} // serve until killed (tray shell replaces this later)
}

// loadSTT picks the engine family from the directory contents: a joiner*.onnx means a
// transducer (parakeet); otherwise whisper encoder/decoder is assumed.
func loadSTT(dir, language string, threads int) (*engine.Recognizer, error) {
	if m, _ := filepath.Glob(filepath.Join(dir, "joiner*.onnx")); len(m) > 0 {
		return engine.NewTransducerSTT(dir, threads)
	}
	return engine.NewWhisperSTT(dir, language, threads)
}

// resolveModelDir returns the model directory to load: the explicitly named one, or the first
// directory under modelsRoot matching any of the auto-detect patterns.
func resolveModelDir(modelsRoot, explicit string, patterns []string) string {
	if explicit != "" {
		return filepath.Join(modelsRoot, explicit)
	}
	for _, p := range patterns {
		if m, _ := filepath.Glob(filepath.Join(modelsRoot, p)); len(m) > 0 {
			return m[0]
		}
	}
	return ""
}

func listen(bind string, port int) net.Listener {
	l, err := net.Listen("tcp", fmt.Sprintf("%s:%d", bind, port))
	if err != nil {
		log.Fatalf("listen %s:%d: %v", bind, port, err)
	}
	return l
}

func defaultModelsDir() string {
	if appData := os.Getenv("APPDATA"); appData != "" {
		return filepath.Join(appData, "tts-sst", "models")
	}
	return "models"
}

// ---- mock engines: protocol testing without any models ----

func mockSTT(pcm []byte, f wyoming.AudioFormat, _ string) (string, error) {
	return fmt.Sprintf("mock transcript of %d bytes", len(pcm)), nil
}

func mockTTS(string) ([]byte, wyoming.AudioFormat, error) {
	const rate = 22050
	n := rate * 4 / 10 // 0.4s of 440Hz tone
	pcm := make([]byte, n*2)
	for i := 0; i < n; i++ {
		v := int16(0.2 * 32767 * math.Sin(2*math.Pi*440*float64(i)/rate))
		pcm[i*2] = byte(v)
		pcm[i*2+1] = byte(v >> 8)
	}
	return pcm, wyoming.AudioFormat{Rate: rate, Width: 2, Channels: 1}, nil
}
