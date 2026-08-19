// tts-sst: local Wyoming-protocol speech services for Windows — Whisper STT and Piper-voice TTS
// via sherpa-onnx, hosted behind a system tray icon. Loopback-only by default.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"os"
	"path/filepath"
	"runtime"

	"github.com/TeeJS/tts-stt-windows/internal/config"
	"github.com/TeeJS/tts-stt-windows/internal/engine"
	"github.com/TeeJS/tts-stt-windows/internal/models"
	"github.com/TeeJS/tts-stt-windows/internal/wyoming"
)

// systemRegion is the region half of the Windows locale ("GB"), used only to preselect a matching
// accent in the first-run question. Not persisted: the user's explicit pick is what gets saved.
var systemRegion string

func main() {
	var (
		console    = flag.Bool("console", false, "run without the tray icon (dev/service use)")
		mock       = flag.Bool("mock", false, "serve mock engines (no models needed) for protocol testing; implies -console")
		noDownload = flag.Bool("no-download", false, "never download models; fail if they're missing")
		modelsDir  = flag.String("models", defaultModelsDir(), "models directory")
		bind       = flag.String("bind", "", "override bind address (default from config, 127.0.0.1)")
		sttPort    = flag.Int("stt-port", 0, "override Wyoming ASR port (default from config, 10300)")
		ttsPort    = flag.Int("tts-port", 0, "override Wyoming TTS port (default from config, 10200)")
		language   = flag.String("language", "", "override STT language hint (default from config, en)")
		threads    = flag.Int("threads", 0, "inference threads (0 = auto: ~physical cores, capped at 8)")
	)
	flag.Parse()
	setupLogging()

	cfg := config.Load()
	if !cfg.Setup {
		// Seed the first-run question with the Windows display language, so most users can accept
		// the default instead of hunting through fifty options — and nobody has English assumed
		// for them. Only languages we actually have a voice for are worth pre-selecting.
		if sys, region := config.SystemLocale(); sys != "" {
			if v, _ := models.DefaultsFor(sys); v.ID != "" {
				cfg.Language = sys
				systemRegion = region // the UI preselects a matching accent, e.g. en-GB over en-US
			} else {
				log.Printf("no voice available for the system language %q; first-run question defaults to English", sys)
			}
		}
	}
	if *bind != "" {
		cfg.Bind = *bind
	}
	if *sttPort > 0 {
		cfg.STTPort = *sttPort
	}
	if *ttsPort > 0 {
		cfg.TTSPort = *ttsPort
	}
	if *language != "" {
		cfg.Language = *language
	}
	if *threads > 0 {
		cfg.Threads = *threads
	}
	n := cfg.Threads
	if n <= 0 {
		// Half the logical CPUs approximates physical cores (SMT); onnxruntime scales poorly
		// beyond ~8 threads for these model sizes.
		n = runtime.NumCPU() / 2
		if n < 2 {
			n = 2
		}
		if n > 8 {
			n = 8
		}
	}
	log.Printf("tts-sst starting: bind=%s stt=%d tts=%d threads=%d models=%s", cfg.Bind, cfg.STTPort, cfg.TTSPort, n, *modelsDir)

	if *mock {
		runMock(cfg)
		return
	}
	svc := newService(cfg, *modelsDir, n)
	svc.noDownload = *noDownload
	ui, err := startUI(svc)
	if err != nil {
		log.Printf("settings UI unavailable: %v", err) // not fatal: the speech services matter more
	}
	if *console {
		svc.Start()
		if ui != nil {
			log.Printf("settings: %s", ui.URL())
		}
		select {}
	}
	runTray(svc, ui)
}

// setupLogging tees the log to %APPDATA%\tts-sst\tts-sst.log — the only place to look when
// running as a windowless tray app. Truncated when it grows past 5MB.
func setupLogging() {
	log.SetFlags(log.Ldate | log.Ltime)
	p := logPath()
	if fi, err := os.Stat(p); err == nil && fi.Size() > 5*1024*1024 {
		os.Remove(p)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return // stdout-only is fine; never block startup on logging
	}
	log.SetOutput(io.MultiWriter(os.Stdout, f))
}

func logPath() string { return filepath.Join(config.Dir(), "tts-sst.log") }

func defaultModelsDir() string {
	return filepath.Join(config.Dir(), "models")
}

// loadSTT picks the engine family from the directory contents: a joiner*.onnx means a
// transducer (parakeet); otherwise whisper encoder/decoder is assumed.
func loadSTT(dir, language string, threads int) (*engine.Recognizer, error) {
	if m, _ := filepath.Glob(filepath.Join(dir, "joiner*.onnx")); len(m) > 0 {
		return engine.NewTransducerSTT(dir, threads)
	}
	return engine.NewWhisperSTT(dir, language, "transcribe", threads)
}

// ---- mock engines: protocol testing without models or a tray ----

func runMock(cfg config.Config) {
	info := wyoming.StaticInfo(wyoming.BuildInfo("mock", "mock"))
	sttL, err := net.Listen("tcp", fmt.Sprintf("%s:%d", cfg.Bind, cfg.STTPort))
	if err != nil {
		log.Fatal(err)
	}
	ttsL, err := net.Listen("tcp", fmt.Sprintf("%s:%d", cfg.Bind, cfg.TTSPort))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("mock STT on %s, mock TTS on %s", sttL.Addr(), ttsL.Addr())
	go wyoming.ServeSTT(sttL, mockSTT, info, log.Printf)
	go wyoming.ServeTTS(ttsL, mockTTS, info, log.Printf)
	select {}
}

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
