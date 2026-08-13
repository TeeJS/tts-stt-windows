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

// offID marks a service the user has switched off. Stored in the same config field as a model id
// (like sapiVoiceID) rather than as a separate flag, so "which model, if any" stays one value.
// An EMPTY field still means "pick a default" — only this explicit value disables a service.
const offID = "off"

// The settings page needs one id per service (a single "off" card would be ambiguous about which
// service it turns off), so each list gets its own; both resolve to offID when applied.
const (
	offTTSID = "off-tts"
	offSTTID = "off-stt"
)

func isOff(id string) bool { return id == offID }

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

	// Listeners are held so a service can be switched off at runtime: closing the listener makes
	// its accept loop return, which frees the port instead of leaving something bound that answers
	// with nothing. Guarded by their own mutex, since enabling one blocks on a model load.
	lnMu  sync.Mutex
	sttLn net.Listener
	ttsLn net.Listener
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
	stt, tts := s.ActiveSTT(), s.ActiveTTS()
	if isOff(stt) && isOff(tts) {
		s.setStatus("Both services are off — nothing is being served")
		return
	}
	s.setStatus("Running — %s · %s", label(stt), label(tts))
}

func label(id string) string {
	switch id {
	case offID:
		return "off"
	case sapiVoiceID:
		return "Windows built-in voice"
	case "":
		return "none"
	}
	if m, ok := models.ByID(id); ok {
		if m.Region != "" {
			return m.Name + " (" + m.Region + ")"
		}
		return m.Name
	}
	return id
}

// advertised is the model name to publish in Wyoming discovery, or "" for a service that is off
// so BuildInfo omits it entirely — a client shouldn't be told about a service that isn't running.
func advertised(id string) string {
	if isOff(id) || id == "" {
		return ""
	}
	return label(id)
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
		// so speech is available as early as possible.
		if err := s.useTTS(voiceID); err != nil {
			s.setStatus("Voice load failed: %v", err)
		}
		if err := s.useSTT(sttID); err != nil {
			s.setStatus("Speech model load failed: %v", err)
		}
		s.syncListeners()
		s.running()
	}()
}

// resolve returns id if it names something loadable, otherwise fallback. Keeps a stale or
// hand-edited config from leaving the server with no voice or no speech model. "off" is a
// deliberate choice, not a broken value, so it passes through untouched.
func (s *service) resolve(id string, kind models.Kind, fallback string) string {
	if id == "" {
		return fallback
	}
	if isOff(id) {
		return id
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

// syncListeners brings each Wyoming listener into line with whether its service is on: opened
// when a service is active, closed (freeing the port) when it is off. Safe to call repeatedly —
// it only acts on a service whose state actually changed.
func (s *service) syncListeners() {
	s.lnMu.Lock()
	defer s.lnMu.Unlock()

	// The advertised model names change with every switch, so discovery data is rebuilt here
	// rather than captured once at startup.
	info := wyoming.BuildInfo(advertised(s.ActiveSTT()), advertised(s.ActiveTTS()))

	if want := !isOff(s.ActiveSTT()); want != (s.sttLn != nil) {
		if want {
			ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", s.cfg.Bind, s.cfg.STTPort))
			if err != nil {
				s.setStatus("Speech port %d busy: %v", s.cfg.STTPort, err)
			} else {
				s.sttLn = ln
				log.Printf("STT listening on %s", ln.Addr())
				go wyoming.ServeSTT(ln, s.transcribe, info, log.Printf)
			}
		} else {
			log.Printf("STT stopped listening on %s", s.sttLn.Addr())
			s.sttLn.Close() // the accept loop returns on net.ErrClosed
			s.sttLn = nil
		}
	}

	if want := !isOff(s.ActiveTTS()); want != (s.ttsLn != nil) {
		if want {
			ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", s.cfg.Bind, s.cfg.TTSPort))
			if err != nil {
				s.setStatus("Voice port %d busy: %v", s.cfg.TTSPort, err)
			} else {
				s.ttsLn = ln
				log.Printf("TTS listening on %s", ln.Addr())
				go wyoming.ServeTTS(ln, s.synthesize, info, log.Printf)
			}
		} else {
			log.Printf("TTS stopped listening on %s", s.ttsLn.Addr())
			s.ttsLn.Close()
			s.ttsLn = nil
		}
	}
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
	// Drop what the model wrote about sounds rather than speech — "(clicking)", "[BLANK_AUDIO]",
	// stock phrases hallucinated on near-silence. Without this a keyboard click becomes a turn.
	if s.cfg.FilterNonSpeech {
		if clean := engine.CleanTranscript(text); clean != text {
			log.Printf("filtered non-speech: %q -> %q", text, clean)
			text = clean
		}
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

// useSTT installs (downloading on demand) and activates a speech model by catalog id, or unloads
// the engine entirely when switched off — which is where the model's memory is actually returned.
func (s *service) useSTT(id string) error {
	if id == "" {
		return fmt.Errorf("no speech model chosen")
	}
	if isOff(id) {
		s.mu.Lock()
		old := s.stt
		s.stt, s.sttID = nil, offID
		s.mu.Unlock()
		closeAsync(old)
		return nil
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

// useTTS activates a voice by catalog id, or the SAPI built-in, downloading if needed. "off"
// unloads the current voice and frees its memory.
func (s *service) useTTS(id string) error {
	if id == "" {
		return fmt.Errorf("no voice chosen")
	}
	if isOff(id) {
		s.mu.Lock()
		old := s.tts
		s.tts, s.ttsID = nil, offID
		s.mu.Unlock()
		closeAsync(old)
		return nil
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

// SwitchSTT / SwitchTTS are the UI entry points: swap engines, bring the listener into line with
// whether the service is on, and persist the pick.
func (s *service) SwitchSTT(id string) error {
	if err := s.useSTT(id); err != nil {
		s.setStatus("Switch failed: %v", err)
		return err
	}
	s.cfg.STTModel = id
	config.Save(s.cfg)
	s.syncListeners()
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
	s.syncListeners()
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

// SetFilterNonSpeech turns the "(clicking)" / "[BLANK_AUDIO]" filter on or off.
func (s *service) SetFilterNonSpeech(on bool) {
	s.cfg.FilterNonSpeech = on
	config.Save(s.cfg)
}

// CompleteSetup answers the language question — on first launch, or again later from Settings —
// and loads a voice and speech model for the chosen language. Clearing the current picks is what
// makes a re-run meaningful: otherwise it would keep the models chosen for the old language.
func (s *service) CompleteSetup(voice string) {
	s.cfg.Setup = true
	s.cfg.TTSVoice = voice // "" leaves Start to pick a default for the language
	s.cfg.STTModel = ""
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
