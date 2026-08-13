package main

import (
	"fmt"
	"log"
	"net"
	"path/filepath"
	"sync"

	"github.com/TeeJS/tts-stt-windows/internal/config"
	"github.com/TeeJS/tts-stt-windows/internal/engine"
	"github.com/TeeJS/tts-stt-windows/internal/models"
	"github.com/TeeJS/tts-stt-windows/internal/wyoming"
)

// service owns the engines and their Wyoming listeners, and supports live engine swaps from the
// tray menu. The closures handed to ServeSTT/ServeTTS read the current engine through the mutex,
// so a swap never requires touching the listeners.
type service struct {
	cfg       config.Config
	modelsDir string
	threads   int

	noDownload bool

	mu       sync.RWMutex
	stt      *engine.Recognizer
	tts      engine.TTS // *engine.PiperTTS or *engine.SapiTTS
	sttName  string
	ttsName  string
	status   string
	onChange func() // tray refresh hook; nil in console mode

	listenOnce sync.Once
}

func newService(cfg config.Config, modelsDir string, threads int) *service {
	return &service{cfg: cfg, modelsDir: modelsDir, threads: threads, status: "Starting…"}
}

func (s *service) setStatus(format string, args ...any) {
	s.mu.Lock()
	s.status = fmt.Sprintf(format, args...)
	s.mu.Unlock()
	log.Printf("status: %s", fmt.Sprintf(format, args...))
	if s.onChange != nil {
		s.onChange()
	}
}

func (s *service) Status() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status
}

func (s *service) ActiveSTT() string { s.mu.RLock(); defer s.mu.RUnlock(); return s.sttName }
func (s *service) ActiveTTS() string { s.mu.RLock(); defer s.mu.RUnlock(); return s.ttsName }

// Start downloads defaults if missing, loads both engines, and brings the listeners up.
// Runs in its own goroutine; progress lands in the status line.
func (s *service) Start() {
	go func() {
		if !s.noDownload {
			prog := func(name string, done, total int64) {
				s.setStatus("Downloading %s — %d%% of %d MB", name, done*100/total, total>>20)
			}
			if err := models.EnsureDefaults(s.modelsDir, prog); err != nil {
				s.setStatus("Model download failed: %v", err)
				return
			}
		}
		sttPick, ttsPick := s.cfg.STTModel, s.cfg.TTSVoice
		if sttPick == "" {
			sttPick = "whisper-small-en"
		}
		if ttsPick == "" {
			ttsPick = "piper-lessac-medium"
		}
		if err := s.useTTS(ttsPick); err != nil { // voice first: much faster load, speech works sooner
			s.setStatus("Voice load failed: %v", err)
			return
		}
		if err := s.useSTT(sttPick); err != nil {
			s.setStatus("STT load failed: %v", err)
			return
		}
		s.listen()
		s.setStatus("Running — STT %s · voice %s", s.ActiveSTT(), s.ActiveTTS())
	}()
}

// listen starts both Wyoming listeners exactly once.
func (s *service) listen() {
	s.listenOnce.Do(func() {
		info := wyoming.BuildInfo(s.ActiveSTT(), s.ActiveTTS())
		sttL, err := net.Listen("tcp", fmt.Sprintf("%s:%d", s.cfg.Bind, s.cfg.STTPort))
		if err != nil {
			s.setStatus("STT port %d busy: %v", s.cfg.STTPort, err)
			return
		}
		ttsL, err := net.Listen("tcp", fmt.Sprintf("%s:%d", s.cfg.Bind, s.cfg.TTSPort))
		if err != nil {
			s.setStatus("TTS port %d busy: %v", s.cfg.TTSPort, err)
			return
		}
		log.Printf("STT listening on %s, TTS listening on %s", sttL.Addr(), ttsL.Addr())
		go wyoming.ServeSTT(sttL, s.transcribe, info, log.Printf)
		go wyoming.ServeTTS(ttsL, s.synthesize, info, log.Printf)
	})
}

func (s *service) transcribe(pcm []byte, f wyoming.AudioFormat, _ string) (string, error) {
	s.mu.RLock()
	rec := s.stt
	s.mu.RUnlock()
	if rec == nil {
		return "", fmt.Errorf("no STT engine loaded")
	}
	return rec.Transcribe(pcm, f.Rate)
}

func (s *service) synthesize(text string) ([]byte, wyoming.AudioFormat, error) {
	s.mu.RLock()
	voice := s.tts
	s.mu.RUnlock()
	if voice == nil {
		return nil, wyoming.AudioFormat{}, fmt.Errorf("no TTS voice loaded")
	}
	pcm, rate, err := voice.Synthesize(text)
	return pcm, wyoming.AudioFormat{Rate: rate, Width: 2, Channels: 1}, err
}

// useSTT installs (downloading on demand) and activates the named registry STT model.
func (s *service) useSTT(name string) error {
	m, err := registryModel(name, models.STT)
	if err != nil {
		return err
	}
	if err := s.ensure(m); err != nil {
		return err
	}
	rec, err := loadSTT(filepath.Join(s.modelsDir, m.Dir), s.cfg.Language, s.threads)
	if err != nil {
		return err
	}
	s.mu.Lock()
	old := s.stt
	s.stt, s.sttName = rec, m.Name
	s.mu.Unlock()
	if old != nil {
		go old.Close() // Close waits out any in-flight Transcribe before freeing
	}
	return nil
}

// sapiVoiceName is the pseudo-registry entry for the built-in Windows fallback voice: not
// downloadable, so it's never in models.Registry, but selectable from the same tray menu.
const sapiVoiceName = "windows-builtin"

// useTTS activates the named voice — either a downloadable registry entry, or the SAPI fallback,
// which needs no download and cannot fail to become available (Windows always has it).
func (s *service) useTTS(name string) error {
	var voice engine.TTS
	if name == sapiVoiceName {
		voice = engine.NewSapiTTS()
	} else {
		m, err := registryModel(name, models.TTS)
		if err != nil {
			return err
		}
		if err := s.ensure(m); err != nil {
			return err
		}
		v, err := engine.NewPiperTTS(filepath.Join(s.modelsDir, m.Dir), s.threads)
		if err != nil {
			return err
		}
		voice = v
	}
	s.mu.Lock()
	old := s.tts
	s.tts, s.ttsName = voice, name
	s.mu.Unlock()
	closeAsync(old)
	return nil
}

// closer matches both engine.PiperTTS.Close and engine.SapiTTS.Close (neither is part of the
// engine.TTS interface itself — SAPI's Close is a no-op, Piper's frees a C++ object).
type closer interface{ Close() }

func closeAsync(v any) {
	if c, ok := v.(closer); ok {
		go c.Close()
	}
}

// SwitchSTT / SwitchTTS are the tray-menu entry points: swap engines, persist the pick.
func (s *service) SwitchSTT(name string) {
	s.setStatus("Loading %s…", name)
	if err := s.useSTT(name); err != nil {
		s.setStatus("Switch failed: %v", err)
		return
	}
	s.cfg.STTModel = name
	config.Save(s.cfg)
	s.setStatus("Running — STT %s · voice %s", s.ActiveSTT(), s.ActiveTTS())
}

func (s *service) SwitchTTS(name string) {
	s.setStatus("Loading %s…", name)
	if err := s.useTTS(name); err != nil {
		s.setStatus("Switch failed: %v", err)
		return
	}
	s.cfg.TTSVoice = name
	config.Save(s.cfg)
	s.setStatus("Running — STT %s · voice %s", s.ActiveSTT(), s.ActiveTTS())
}

func (s *service) ensure(m models.Model) error {
	if models.Installed(s.modelsDir, m) {
		return nil
	}
	if s.noDownload {
		return fmt.Errorf("model %s not installed and downloads are disabled", m.Name)
	}
	return models.Install(s.modelsDir, m, func(name string, done, total int64) {
		s.setStatus("Downloading %s — %d%% of %d MB", name, done*100/total, total>>20)
	})
}

func registryModel(name string, kind models.Kind) (models.Model, error) {
	for _, m := range models.Registry {
		if m.Name == name && m.Kind == kind {
			return m, nil
		}
	}
	return models.Model{}, fmt.Errorf("unknown %s model %q", kind, name)
}
