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

// sapiVoiceID is the built-in Windows voice: not in the catalog (nothing to download) but
// selectable like any other, and the only voice guaranteed to exist before any download.
const sapiVoiceID = "windows-builtin"

// service owns the engines and their Wyoming listeners, and supports live engine swaps from the
// settings UI. The closures handed to ServeSTT/ServeTTS read the current engine through the mutex,
// so a swap never requires touching the listeners.
type service struct {
	cfg        config.Config
	modelsDir  string
	threads    int
	noDownload bool

	mu       sync.RWMutex
	stt      *engine.Recognizer
	tts      engine.TTS // *engine.PiperTTS or *engine.SapiTTS
	sttID    string
	ttsID    string
	status   string
	busy     string // non-empty while a download/load is in flight, for the UI
	onChange func() // tray/UI refresh hook

	listenOnce sync.Once
}

func newService(cfg config.Config, modelsDir string, threads int) *service {
	return &service{cfg: cfg, modelsDir: modelsDir, threads: threads, status: "Starting…"}
}

func (s *service) setStatus(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	s.mu.Lock()
	s.status = msg
	s.mu.Unlock()
	log.Printf("status: %s", msg)
	if s.onChange != nil {
		s.onChange()
	}
}

func (s *service) Status() string    { s.mu.RLock(); defer s.mu.RUnlock(); return s.status }
func (s *service) Busy() string      { s.mu.RLock(); defer s.mu.RUnlock(); return s.busy }
func (s *service) ActiveSTT() string { s.mu.RLock(); defer s.mu.RUnlock(); return s.sttID }
func (s *service) ActiveTTS() string { s.mu.RLock(); defer s.mu.RUnlock(); return s.ttsID }

func (s *service) setBusy(what string) {
	s.mu.Lock()
	s.busy = what
	s.mu.Unlock()
	if s.onChange != nil {
		s.onChange()
	}
}

// running composes the steady-state status line from whatever is actually loaded.
func (s *service) running() {
	voice, stt := s.ActiveTTS(), s.ActiveSTT()
	s.setStatus("Running — %s · %s", label(stt), label(voice))
}

func label(id string) string {
	if id == sapiVoiceID {
		return "Windows built-in voice"
	}
	if m, ok := models.ByID(id); ok {
		if m.Region != "" {
			return m.Name + " (" + m.Region + ")"
		}
		return m.Name
	}
	if id == "" {
		return "none"
	}
	return id
}

// Start loads the configured engines (downloading them if needed) and brings the listeners up.
// Runs in its own goroutine; progress lands in the status line.
//
// On a first run it does nothing until the language question is answered: downloading an English
// voice and then a second one for the user's actual language would waste hundreds of megabytes
// and minutes of their time.
func (s *service) Start() {
	if !s.cfg.Setup {
		s.setStatus("Waiting — choose your language in Settings")
		return
	}
	go func() {
		// Fall back to language defaults when nothing is configured yet, and equally when the
		// configured id is no longer in the catalog — ids change when the catalog is regenerated,
		// and an upgrade must not leave the user mute.
		defVoice, defSTT := models.DefaultsFor(s.cfg.Language)
		voiceID := s.resolve(s.cfg.TTSVoice, models.TTS, defVoice.ID)
		sttID := s.resolve(s.cfg.STTModel, models.STT, defSTT.ID)
		// Voice first: it loads in seconds where a speech model can take a minute to download,
		// so the panel can talk back as early as possible.
		if err := s.useTTS(voiceID); err != nil {
			s.setStatus("Voice load failed: %v", err)
		}
		if err := s.useSTT(sttID); err != nil {
			s.setStatus("Speech model load failed: %v", err)
		}
		s.listen()
		s.running()
	}()
}

// resolve returns id if it names something loadable, otherwise fallback. Keeps a stale or
// hand-edited config from leaving the server with no voice or no speech model.
func (s *service) resolve(id string, kind models.Kind, fallback string) string {
	if id == "" {
		return fallback
	}
	if kind == models.TTS && id == sapiVoiceID {
		return id
	}
	if m, ok := models.ByID(id); ok && m.Kind == kind {
		return id
	}
	log.Printf("configured %s %q is not in the catalog; falling back to %q", kind, id, fallback)
	return fallback
}

// listen starts both Wyoming listeners exactly once.
func (s *service) listen() {
	s.listenOnce.Do(func() {
		info := wyoming.BuildInfo(label(s.ActiveSTT()), label(s.ActiveTTS()))
		sttL, err := net.Listen("tcp", fmt.Sprintf("%s:%d", s.cfg.Bind, s.cfg.STTPort))
		if err != nil {
			s.setStatus("Speech port %d busy: %v", s.cfg.STTPort, err)
			return
		}
		ttsL, err := net.Listen("tcp", fmt.Sprintf("%s:%d", s.cfg.Bind, s.cfg.TTSPort))
		if err != nil {
			s.setStatus("Voice port %d busy: %v", s.cfg.TTSPort, err)
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
		return "", fmt.Errorf("no speech model loaded")
	}
	text, err := rec.Transcribe(pcm, f.Rate)
	if err != nil {
		return "", err
	}
	return text, nil
}

func (s *service) synthesize(text string) ([]byte, wyoming.AudioFormat, error) {
	s.mu.RLock()
	voice := s.tts
	s.mu.RUnlock()
	if voice == nil {
		return nil, wyoming.AudioFormat{}, fmt.Errorf("no voice loaded")
	}
	pcm, rate, err := voice.Synthesize(text)
	return pcm, wyoming.AudioFormat{Rate: rate, Width: 2, Channels: 1}, err
}

// useSTT installs (downloading on demand) and activates a speech model by catalog id.
func (s *service) useSTT(id string) error {
	if id == "" {
		return fmt.Errorf("no speech model chosen")
	}
	m, ok := models.ByID(id)
	if !ok || m.Kind != models.STT {
		return fmt.Errorf("unknown speech model %q", id)
	}
	if err := s.ensure(m); err != nil {
		return err
	}
	s.setBusy("Loading " + m.Name + "…")
	defer s.setBusy("")
	rec, err := engine.NewSTT(filepath.Join(s.modelsDir, m.Dir), m.Family, sttLanguage(m, s.cfg.Language), s.threads)
	if err != nil {
		return err
	}
	s.mu.Lock()
	old := s.stt
	s.stt, s.sttID = rec, m.ID
	s.mu.Unlock()
	closeAsync(old) // Close waits out any in-flight Transcribe before freeing
	return nil
}

// sttLanguage is the language hint passed to models that accept one. Multi-language models take
// the user's language; single-language models must be left empty (their own language is baked in,
// and a mismatched hint makes whisper translate instead of transcribe).
func sttLanguage(m models.Model, userLang string) string {
	for _, l := range m.Langs {
		if l == "multi" {
			return userLang
		}
	}
	return ""
}

// useTTS activates a voice by catalog id, or the SAPI built-in, downloading if needed.
func (s *service) useTTS(id string) error {
	if id == "" {
		return fmt.Errorf("no voice chosen")
	}
	var voice engine.TTS
	if id == sapiVoiceID {
		voice = engine.NewSapiTTS()
	} else {
		m, ok := models.ByID(id)
		if !ok || m.Kind != models.TTS {
			return fmt.Errorf("unknown voice %q", id)
		}
		if err := s.ensure(m); err != nil {
			return err
		}
		s.setBusy("Loading " + m.Name + "…")
		defer s.setBusy("")
		v, err := engine.NewPiperTTS(filepath.Join(s.modelsDir, m.Dir), s.threads)
		if err != nil {
			return err
		}
		v.SetSpeed(s.cfg.Speed)
		voice = v
	}
	s.mu.Lock()
	old := s.tts
	s.tts, s.ttsID = voice, id
	s.mu.Unlock()
	closeAsync(old)
	return nil
}

// SwitchSTT / SwitchTTS are the UI entry points: swap engines, persist the pick.
func (s *service) SwitchSTT(id string) error {
	if err := s.useSTT(id); err != nil {
		s.setStatus("Switch failed: %v", err)
		return err
	}
	s.cfg.STTModel = id
	config.Save(s.cfg)
	s.running()
	return nil
}

func (s *service) SwitchTTS(id string) error {
	if err := s.useTTS(id); err != nil {
		s.setStatus("Switch failed: %v", err)
		return err
	}
	s.cfg.TTSVoice = id
	config.Save(s.cfg)
	s.running()
	return nil
}

// SetSpeed persists and applies the speaking rate to the live voice.
func (s *service) SetSpeed(speed float32) {
	s.cfg.Speed = speed
	config.Save(s.cfg)
	s.mu.RLock()
	v, ok := s.tts.(*engine.PiperTTS)
	s.mu.RUnlock()
	if ok {
		v.SetSpeed(speed)
	}
}

// SetLanguage persists the user's language; it drives multi-language model hints and default picks.
func (s *service) SetLanguage(lang string) {
	s.cfg.Language = lang
	config.Save(s.cfg)
}

// CompleteSetup marks the first-run question answered and starts loading models for the language
// that was chosen.
func (s *service) CompleteSetup() {
	s.cfg.Setup = true
	config.Save(s.cfg)
	s.Start()
}

func (s *service) ensure(m models.Model) error {
	if models.Installed(s.modelsDir, m) {
		return nil
	}
	if s.noDownload {
		return fmt.Errorf("%s is not installed and downloads are disabled", m.Name)
	}
	s.setBusy("Downloading " + m.Name + "…")
	defer s.setBusy("")
	return models.Install(s.modelsDir, m, func(id string, done, total int64) {
		s.setBusy(fmt.Sprintf("Downloading %s — %d%%", m.Name, pct(done, total)))
		s.setStatus("Downloading %s — %d%% of %d MB", m.Name, pct(done, total), total>>20)
	})
}

func pct(done, total int64) int {
	if total <= 0 {
		return 0
	}
	return int(done * 100 / total)
}

// closer matches Close on every engine type (it isn't part of the engine.TTS/STT interfaces —
// SAPI's Close is a no-op, the sherpa-backed ones free C++ objects). Every implementation is
// nil-receiver safe, which matters because `v` is the previously-active engine and is a typed
// nil on the first load — and a nil pointer inside an interface is not itself nil.
type closer interface{ Close() }

func closeAsync(v any) {
	if c, ok := v.(closer); ok {
		go c.Close()
	}
}
