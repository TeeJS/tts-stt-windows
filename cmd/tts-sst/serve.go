package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/TeeJS/tts-stt-windows/internal/config"
	"github.com/TeeJS/tts-stt-windows/internal/diarize"
	"github.com/TeeJS/tts-stt-windows/internal/engine"
	"github.com/TeeJS/tts-stt-windows/internal/localvoices"
	"github.com/TeeJS/tts-stt-windows/internal/models"
	"github.com/TeeJS/tts-stt-windows/internal/voices"
	"github.com/TeeJS/tts-stt-windows/internal/wyoming"
)

// sapiVoiceID is the built-in Windows voice: not in the catalog (nothing to download) but
// selectable like any other, and the only voice guaranteed to exist before any download.
const sapiVoiceID = "windows-builtin"

// pocketModelID is the catalog entry for the voice-cloning model. It is never selected directly:
// the user picks a cloned voice ("pocket:<name>", see internal/voices) and the model follows.
const pocketModelID = "pocket-tts"

// voicesDir holds the reference clips cloned voices are built from — one WAV per voice; copy
// the folder to back them up or move machines.
func voicesDir() string { return filepath.Join(config.Dir(), "voices") }

// localVoicesDir holds user-supplied Piper voices, one folder each (see internal/localvoices).
func localVoicesDir() string { return filepath.Join(config.Dir(), "piper") }

// offID marks a service the user has switched off. Stored in the same config field as a model id
// (like sapiVoiceID) rather than as a separate flag, so "which model, if any" stays one value.
// An EMPTY field still means "pick a default" — only this explicit value disables a service.
const offID = "off"

// The settings page needs one id per service (a single "off" card would be ambiguous about which
// service it turns off), so each list gets its own; both resolve to offID when applied.
const (
	offTTSID = "off-tts"
	offSTTID = "off-stt"
	// "On" rows in the settings page: re-enable a service with its previous model.
	onTTSID = "on-tts"
	onSTTID = "on-stt"
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
	switchMu sync.Mutex // serializes whole TTS switches: a clone swap reads the live engine, then reinstalls it
	stt      *engine.Recognizer
	tts      engine.TTS // the active voice: *engine.PiperTTS, *engine.PocketTTS or *engine.SapiTTS
	sttID    string
	ttsID    string
	// alt is the other kind of voice engine, loaded only while cfg.MixVoices is on: a standard
	// voice when a clone is active and vice versa, so a request can name either (see synthesize).
	alt      engine.TTS
	altID    string
	status   string
	busy     string // non-empty while a download/load is in flight, for the UI
	onChange func() // tray/UI refresh hook

	// The meetings service (batch diarization over HTTP) — nil while off.
	diar    *diarize.Diarizer
	diarSvc *diarize.Service
	diarRec *engine.Recognizer // dedicated transcriber; nil when sharing the live STT

	// Listeners are held so a service can be switched off at runtime: closing the listener makes
	// its accept loop return, which frees the port instead of leaving something bound that answers
	// with nothing. Guarded by their own mutex, since enabling one blocks on a model load.
	lnMu   sync.Mutex
	sttLn  net.Listener
	ttsLn  net.Listener
	meetLn net.Listener
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

// AltTTS is the voice held by the second engine while mixing is on, or "".
func (s *service) AltTTS() string { s.mu.RLock(); defer s.mu.RUnlock(); return s.altID }

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
	if isOff(stt) && isOff(tts) && !s.meetingsOn() {
		s.setStatus("All services are off — nothing is being served")
		return
	}
	line := fmt.Sprintf("Running — %s · %s", label(stt), label(tts))
	if alt := s.AltTTS(); alt != "" {
		line += " + " + label(alt)
	}
	if s.meetingsOn() {
		// Report what is true: the port-busy failure used to be overwritten by
		// this line claiming meetings was being served.
		s.lnMu.Lock()
		listening := s.meetLn != nil
		s.lnMu.Unlock()
		if listening {
			line += fmt.Sprintf(" · meetings :%d", s.cfg.MeetingsPort)
		} else {
			line += fmt.Sprintf(" · meetings port %d BUSY", s.cfg.MeetingsPort)
		}
	}
	s.setStatus("%s", line)
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
	if name, ok := voices.Name(id); ok {
		return name + " (cloned)"
	}
	if name, ok := localvoices.Name(id); ok {
		if v, ok := localvoices.Get(localVoicesDir(), name); ok {
			return v.Label
		}
		return name
	}
	if m, ok := models.ByID(id); ok {
		if m.Region != "" {
			return m.Name + " (" + m.Region + ")"
		}
		return m.Name
	}
	return id
}

// info answers a `describe` with what is true right now — which services are actually running and
// which model each is using.
func (s *service) info() wyoming.Info {
	s.mu.RLock()
	active, alt, altID := s.ttsID, s.alt, s.altID
	_, pocketLoaded := s.tts.(*engine.PocketTTS)
	if _, ok := alt.(*engine.PocketTTS); ok {
		pocketLoaded = true
	}
	s.mu.RUnlock()
	ttsVoices := []string{advertised(active)}
	// Advertise by the name a client must send back as voice.name: clones by their pocket:<name>
	// id (every clone, while any loaded engine is PocketTTS), local voices by their local:<name>
	// id (their label is not unique), the second engine's standard voice by its catalog id.
	if _, cloned := voices.Name(active); cloned {
		ttsVoices[0] = active
	} else if _, local := localvoices.Name(active); local {
		ttsVoices[0] = active
	}
	if pocketLoaded {
		if store, err := voices.NewStore(voicesDir()); err == nil {
			if names, err := store.List(); err == nil {
				for _, n := range names {
					if id := voices.ID(n); id != active {
						ttsVoices = append(ttsVoices, id)
					}
				}
			}
		}
	}
	if _, cloned := voices.Name(altID); alt != nil && !cloned {
		ttsVoices = append(ttsVoices, altID)
	}
	return wyoming.BuildInfo(advertised(s.ActiveSTT()), ttsVoices...)
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
		if s.meetingsOn() {
			if err := s.useMeetings(true); err != nil {
				s.setStatus("Meetings service load failed: %v", err)
			}
		}
		s.syncListeners()
		s.syncMeetingsListener()
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
	if name, ok := voices.Name(id); ok && kind == models.TTS {
		if store, err := voices.NewStore(voicesDir()); err == nil && store.Exists(name) {
			return id
		}
		log.Printf("configured cloned voice %q has no clip in %s; falling back to %q", name, voicesDir(), fallback)
		return fallback
	}
	if name, ok := localvoices.Name(id); ok && kind == models.TTS {
		if localvoices.Exists(localVoicesDir(), name) {
			return id
		}
		log.Printf("configured local voice %q has no %s.onnx + %s.onnx.json in %s; falling back to %q", name, name, name, localVoicesDir(), fallback)
		return fallback
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

	// Discovery is answered from current state on every request, not from a snapshot: a listener
	// created while both services were on would otherwise keep telling clients about the other
	// service long after it was switched off and its port closed.
	info := s.info

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

func (s *service) synthesize(text, requested string) ([]byte, wyoming.AudioFormat, error) {
	s.mu.RLock()
	voice, activeID, alt, altID := s.tts, s.ttsID, s.alt, s.altID
	s.mu.RUnlock()
	if voice == nil {
		return nil, wyoming.AudioFormat{}, fmt.Errorf("no voice loaded")
	}
	t0 := time.Now()
	var (
		pcm  []byte
		rate int
		err  error
	)
	// A client may name any voice `describe` advertised. A clone is served by whichever loaded
	// engine is PocketTTS (the clip is swapped for this request only); the standard voice in the
	// second engine is served by that engine. Anything else — an unknown name, or a kind of
	// voice no loaded engine can speak — falls back to the active voice, which is what every
	// client got before per-request voices existed.
	if requested != "" && requested != activeID && requested != label(activeID) {
		if name, ok := voices.Name(requested); ok {
			p, _ := voice.(*engine.PocketTTS)
			if p == nil {
				p, _ = alt.(*engine.PocketTTS)
			}
			if p != nil {
				if store, serr := voices.NewStore(voicesDir()); serr == nil {
					if ref, refRate, lerr := store.Load(name); lerr == nil {
						pcm, rate, err = p.SynthesizeWith(text, ref, refRate)
						goto done
					}
				}
			}
		} else if alt != nil && (requested == altID || requested == label(altID)) {
			pcm, rate, err = alt.Synthesize(text)
			goto done
		}
		log.Printf("tts: requested voice %q unavailable, using %s", requested, label(activeID))
	}
	pcm, rate, err = voice.Synthesize(text)
done:
	if err == nil && rate > 0 {
		audio := float64(len(pcm)/2) / float64(rate)
		log.Printf("tts: %.1fs audio in %v (%.2fx realtime) for %d chars", audio, time.Since(t0).Round(time.Millisecond), audio/time.Since(t0).Seconds(), len(text))
	}
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
	rec, err := engine.NewSTT(filepath.Join(s.modelsDir, m.Dir), m.Family, sttLanguage(m, s.sttHint()), s.threads)
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

// sttHint is the user language passed on to multi-language models — or "" (auto-detect per
// utterance) when the user said the machine will hear languages other than their own
// (SttAutoLanguage, the first-run "used for translation" answer).
func (s *service) sttHint() string {
	if s.cfg.SttAutoLanguage {
		return ""
	}
	return s.cfg.Language
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
	s.switchMu.Lock()
	defer s.switchMu.Unlock()
	if isOff(id) {
		s.mu.Lock()
		old, oldAlt := s.tts, s.alt
		s.tts, s.ttsID, s.alt, s.altID = nil, offID, nil, ""
		s.mu.Unlock()
		closeAsync(old)
		closeAsync(oldAlt)
		return nil
	}
	s.mu.RLock()
	cur, curID, alt, altID := s.tts, s.ttsID, s.alt, s.altID
	s.mu.RUnlock()
	var voice engine.TTS
	if alt != nil && (id == altID || sameKind(id, altID)) {
		// The requested voice lives in the other engine (or is a clone and the other engine is
		// PocketTTS): promote that engine instead of loading a second copy. The one being
		// demoted becomes alt, or is closed by syncAlt if mixing is off.
		voice = alt
		if name, ok := voices.Name(id); ok {
			if _, err := s.usePocketVoice(name, voice.(*engine.PocketTTS)); err != nil {
				return err
			}
		}
		s.mu.Lock()
		s.tts, s.ttsID, s.alt, s.altID = voice, id, cur, curID
		s.mu.Unlock()
		s.syncAlt()
		return nil
	}
	if name, ok := voices.Name(id); ok {
		live, _ := cur.(*engine.PocketTTS)
		v, err := s.usePocketVoice(name, live)
		if err != nil {
			return err
		}
		voice = v
	} else {
		v, err := s.buildStandard(id)
		if err != nil {
			return err
		}
		voice = v
	}
	s.mu.Lock()
	old := s.tts
	s.tts, s.ttsID = voice, id
	s.mu.Unlock()
	if old != voice { // switching between two cloned voices keeps the same PocketTTS engine
		closeAsync(old)
	}
	s.syncAlt()
	return nil
}

// sameKind reports whether two voice ids use the same engine family: both clones (one
// PocketTTS engine serves every clip) or both standard voices with the same id. Local Piper
// voices are standard voices here — each gets its own engine, like a catalog voice.
func sameKind(a, b string) bool {
	_, ca := voices.Name(a)
	_, cb := voices.Name(b)
	return ca && cb
}

// buildStandard loads a catalog (Piper/Coqui) voice, a local Piper voice, or the Windows
// built-in one.
func (s *service) buildStandard(id string) (engine.TTS, error) {
	if id == sapiVoiceID {
		return engine.NewSapiTTS(), nil
	}
	if name, ok := localvoices.Name(id); ok {
		return s.buildLocal(name)
	}
	m, ok := models.ByID(id)
	if !ok || m.Kind != models.TTS || m.ID == pocketModelID {
		return nil, fmt.Errorf("unknown voice %q", id)
	}
	if err := s.ensure(m); err != nil {
		return nil, err
	}
	s.setBusy("Loading " + m.Name + "…")
	defer s.setBusy("")
	v, err := engine.NewPiperTTS(filepath.Join(s.modelsDir, m.Dir), s.threads)
	if err != nil {
		return nil, err
	}
	v.SetSpeed(s.cfg.Speed)
	return v, nil
}

// buildLocal loads a user-supplied Piper voice from localVoicesDir(). The folder only has the
// Piper export; localvoices.Prepare adds the tokens.txt and borrows espeak-ng-data from an
// installed catalog Piper voice, downloading the language default if none is installed yet.
func (s *service) buildLocal(name string) (engine.TTS, error) {
	v, ok := localvoices.Get(localVoicesDir(), name)
	if !ok {
		return nil, fmt.Errorf("local voice %q: %s.onnx and %s.onnx.json not found in %s", name, name, name, filepath.Join(localVoicesDir(), name))
	}
	espeak, err := s.espeakDataDir()
	if err != nil {
		return nil, fmt.Errorf("local voice %q: %w", name, err)
	}
	dir, err := localvoices.Prepare(localVoicesDir(), name, espeak)
	if err != nil {
		return nil, err
	}
	s.setBusy("Loading " + v.Label + "…")
	defer s.setBusy("")
	eng, err := engine.NewPiperTTS(dir, s.threads)
	if err != nil {
		return nil, err
	}
	eng.SetSpeed(s.cfg.Speed)
	return eng, nil
}

// espeakDataDir finds an espeak-ng-data folder for local Piper voices: from any installed
// catalog Piper voice, else by installing the user's language default (or the English one when
// that language has no Piper voice). Every Piper archive ships the same full espeak data set.
func (s *service) espeakDataDir() (string, error) {
	for _, id := range models.InstalledIDs(s.modelsDir) {
		if m, ok := models.ByID(id); ok && m.Kind == models.TTS && m.Family == "piper" {
			if d := filepath.Join(s.modelsDir, m.Dir, "espeak-ng-data"); isDir(d) {
				return d, nil
			}
		}
	}
	def, _ := models.DefaultsFor(s.cfg.Language)
	if def.Family != "piper" {
		def, _ = models.DefaultsFor("en")
	}
	if def.ID == "" || def.Family != "piper" {
		return "", fmt.Errorf("no catalog Piper voice to borrow espeak-ng-data from")
	}
	if err := s.ensure(def); err != nil {
		return "", fmt.Errorf("espeak-ng-data (from %s): %w", def.Name, err)
	}
	d := filepath.Join(s.modelsDir, def.Dir, "espeak-ng-data")
	if !isDir(d) {
		return "", fmt.Errorf("%s has no espeak-ng-data", def.Name)
	}
	return d, nil
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// syncAlt brings the second engine into line with cfg.MixVoices: loaded with the other kind
// of voice while mixing is on, closed otherwise. Called with switchMu held, after every switch
// and when the setting changes. A failure to load the second engine is logged, never fatal —
// the active voice keeps working.
func (s *service) syncAlt() {
	s.mu.RLock()
	active, alt, altID := s.ttsID, s.alt, s.altID
	s.mu.RUnlock()
	want := ""
	if s.cfg.MixVoices && !isOff(active) && active != "" {
		want = s.otherVoice(active)
	}
	if want == altID && (want == "" || alt != nil) {
		return
	}
	var eng engine.TTS
	if want != "" {
		var err error
		if name, ok := voices.Name(want); ok {
			eng, err = s.usePocketVoice(name, nil)
		} else {
			eng, err = s.buildStandard(want)
		}
		if err != nil {
			log.Printf("mixed voices: could not load %s alongside %s: %v", label(want), label(active), err)
			want = ""
		}
	}
	s.mu.Lock()
	old := s.alt
	s.alt, s.altID = eng, want
	s.mu.Unlock()
	closeAsync(old)
	if want != "" {
		log.Printf("mixed voices: %s loaded alongside %s", label(want), label(active))
	}
}

// otherVoice picks what the second engine should hold given the active voice: the remembered
// standard voice (or the language default) when a clone is active; the remembered clone (or
// the first one in voices\) when a standard voice is active. "" when there is nothing to pair.
func (s *service) otherVoice(active string) string {
	store, err := voices.NewStore(voicesDir())
	if err != nil {
		return ""
	}
	if _, cloned := voices.Name(active); cloned {
		def, _ := models.DefaultsFor(s.cfg.Language)
		fallback := def.ID
		if fallback == "" {
			fallback = sapiVoiceID
		}
		id := s.resolve(s.cfg.StandardVoice, models.TTS, fallback)
		if _, c := voices.Name(id); c || isOff(id) || id == "" {
			return fallback
		}
		return id
	}
	if name, ok := voices.Name(s.cfg.CloneVoice); ok && store.Exists(name) {
		return s.cfg.CloneVoice
	}
	if names, err := store.List(); err == nil && len(names) > 0 {
		return voices.ID(names[0])
	}
	return ""
}

// usePocketVoice prepares a cloned voice: the shared pocket-tts model (downloaded on first use)
// plus the named reference clip from voicesDir(). Given an already-loaded PocketTTS engine only
// the clip changes, which is near-instant — the model stays loaded; nil loads a fresh engine.
func (s *service) usePocketVoice(name string, live *engine.PocketTTS) (engine.TTS, error) {
	store, err := voices.NewStore(voicesDir())
	if err != nil {
		return nil, err
	}
	samples, rate, err := store.Load(name)
	if err != nil {
		return nil, fmt.Errorf("cloned voice %q: %w", name, err)
	}
	if live == nil {
		m, ok := models.ByID(pocketModelID)
		if !ok {
			return nil, fmt.Errorf("%s is not in the catalog", pocketModelID)
		}
		if err := s.ensure(m); err != nil {
			return nil, err
		}
		s.setBusy("Loading " + m.Name + "…")
		defer s.setBusy("")
		t0 := time.Now()
		live, err = engine.NewPocketTTS(filepath.Join(s.modelsDir, m.Dir), s.threads)
		if err != nil {
			return nil, err
		}
		live.SetSpeed(s.cfg.Speed)
		log.Printf("pocket-tts: model loaded in %v", time.Since(t0).Round(time.Millisecond))
	}
	// sherpa resamples the clip to its own 24 kHz internally; pass the stored rate as-is.
	live.SetReference(samples, rate)
	log.Printf("pocket-tts: voice %q (%.1fs clip)", name, float64(len(samples))/float64(rate))
	return live, nil
}

// SwitchSTT / SwitchTTS are the UI entry points: swap engines, bring the listener into line with
// whether the service is on, and persist the pick.
func (s *service) SwitchSTT(id string) error {
	if err := s.useSTT(id); err != nil {
		s.setStatus("Switch failed: %v", err)
		return err
	}
	if isOff(id) && s.cfg.STTModel != "" && !isOff(s.cfg.STTModel) {
		s.cfg.PrevSTTModel = s.cfg.STTModel // so "On" can restore this pick
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
	if isOff(id) && s.cfg.TTSVoice != "" && !isOff(s.cfg.TTSVoice) {
		s.cfg.PrevTTSVoice = s.cfg.TTSVoice
	}
	s.cfg.TTSVoice = id
	// Remember the last voice of each kind: that is what the second engine loads when mixing.
	if _, cloned := voices.Name(id); cloned {
		s.cfg.CloneVoice = id
	} else if !isOff(id) && id != "" {
		s.cfg.StandardVoice = id
	}
	config.Save(s.cfg)
	s.syncListeners()
	s.running()
	return nil
}

// TurnOnSTT / TurnOnTTS re-enable a switched-off service with the model it was
// using before, falling back to the language default if that pick is gone.
func (s *service) TurnOnSTT() error {
	_, def := models.DefaultsFor(s.cfg.Language)
	id := s.resolve(s.cfg.PrevSTTModel, models.STT, def.ID)
	if id == "" || isOff(id) {
		id = def.ID
	}
	return s.SwitchSTT(id)
}

func (s *service) TurnOnTTS() error {
	def, _ := models.DefaultsFor(s.cfg.Language)
	fallback := def.ID
	if fallback == "" {
		fallback = sapiVoiceID // a language with no downloadable voice still has SAPI
	}
	id := s.resolve(s.cfg.PrevTTSVoice, models.TTS, fallback)
	if id == "" || isOff(id) {
		id = fallback
	}
	return s.SwitchTTS(id)
}

// SetSpeed persists and applies the speaking rate to the live voice.
func (s *service) SetSpeed(speed float32) {
	s.cfg.Speed = speed
	config.Save(s.cfg)
	s.mu.RLock()
	engines := []engine.TTS{s.tts, s.alt}
	s.mu.RUnlock()
	for _, e := range engines {
		if v, ok := e.(speedSetter); ok && e != nil {
			v.SetSpeed(speed)
		}
	}
}

// SetMixVoices turns the second voice engine on or off (see Config.MixVoices).
func (s *service) SetMixVoices(on bool) {
	s.cfg.MixVoices = on
	config.Save(s.cfg)
	go func() {
		s.switchMu.Lock()
		defer s.switchMu.Unlock()
		s.syncAlt()
		s.running()
	}()
}

// speedSetter is implemented by the engines with a speaking-rate control (Piper, PocketTTS);
// the Windows built-in voice ignores the slider.
type speedSetter interface{ SetSpeed(float32) }

// SetLanguage persists the user's language; it drives multi-language model hints and default picks.
func (s *service) SetLanguage(lang string) {
	s.cfg.Language = lang
	config.Save(s.cfg)
}

// SetSttAutoLanguage persists the "recognize any language" answer (see config.SttAutoLanguage).
// The active STT keeps its current hint until the next model (re)load — the setup flow that sets
// this always reloads models right after.
func (s *service) SetSttAutoLanguage(on bool) {
	s.cfg.SttAutoLanguage = on
	config.Save(s.cfg)
}

// SetBrowseLanguage remembers which language the settings lists are filtered to. "all" is stored
// as a real choice — someone who deliberately widened the list back out shouldn't find it narrowed
// again on the next visit.
func (s *service) SetBrowseLanguage(lang string) {
	s.cfg.BrowseLanguage = lang
	config.Save(s.cfg)
}

// SetFilterNonSpeech turns the "(clicking)" / "[BLANK_AUDIO]" filter on or off.
func (s *service) SetFilterNonSpeech(on bool) {
	s.cfg.FilterNonSpeech = on
	config.Save(s.cfg)
}

// CompleteSetup answers the first-launch questions — language plus which services to
// run — and loads models for the chosen language. Clearing the current picks is what
// makes a re-run meaningful: otherwise it would keep the models chosen for the old
// language. A service the user left unchecked is stored as an explicit "off", not a
// default, so nothing downloads for it.
func (s *service) CompleteSetup(voice string, tts, stt, meetings bool) {
	s.cfg.Setup = true
	if tts {
		s.cfg.TTSVoice = voice // "" leaves Start to pick a default for the language
	} else {
		s.cfg.TTSVoice = offID
	}
	if stt {
		s.cfg.STTModel = ""
	} else {
		s.cfg.STTModel = offID
	}
	if meetings {
		s.cfg.Meetings = "on"
	} else {
		s.cfg.Meetings = "off"
	}
	config.Save(s.cfg)
	if !meetings {
		// A re-run that unchecks transcription must also stop a running service;
		// Start() only ever brings services up.
		_ = s.useMeetings(false)
		s.syncMeetingsListener()
	}
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
