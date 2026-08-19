package main

import (
	"fmt"
	"log"
	"net"
	"path/filepath"

	"github.com/TeeJS/tts-stt-windows/internal/config"
	"github.com/TeeJS/tts-stt-windows/internal/engine"
	"github.com/TeeJS/tts-stt-windows/internal/models"
	"github.com/TeeJS/tts-stt-windows/internal/wyoming"
)

// The translate endpoint: a SECOND Wyoming STT server (on TranslatePort, default 10302) that runs
// the active Whisper model in "translate" mode — any spoken language comes out as English. It is
// separate from the normal STT service on STTPort precisely so same-language dictation keeps working
// there while a client (e.g. open-quake's Live Translate page) points at 10302 for English.
//
// Whisper-only: sherpa's translate task exists on Whisper alone, so this reuses the active STT model
// and is unavailable unless that model is a Whisper model. It loads a second recognizer (Whisper's
// task is fixed at recognizer construction, so it can't share the transcribe recognizer's object),
// which is why it is off by default and freed when switched off.

// translateOn reports whether the translate endpoint is enabled in config.
func (s *service) translateOn() bool { return s.cfg.Translate == "on" }

// useTranslate builds or tears down the dedicated translate recognizer. It always tears down first,
// so a failed (re)build leaves translate cleanly off rather than serving a stale model.
func (s *service) useTranslate(on bool) error {
	s.mu.Lock()
	old := s.transRec
	s.transRec = nil
	s.mu.Unlock()
	closeAsync(old)
	if !on {
		return nil
	}
	id := s.ActiveSTT()
	if id == "" || isOff(id) {
		return fmt.Errorf("translate needs the Speech service on with a Whisper model")
	}
	m, ok := models.ByID(id)
	if !ok || m.Kind != models.STT {
		return fmt.Errorf("active speech model %q is not in the catalog", id)
	}
	if m.Family != "whisper" {
		return fmt.Errorf("translate needs a Whisper speech model — %q is %s; pick a Whisper model in Speech", m.Name, m.Family)
	}
	if err := s.ensure(m); err != nil {
		return err
	}
	s.setBusy("Loading translation (Whisper → English)…")
	defer s.setBusy("")
	rec, err := engine.NewWhisperTranslate(filepath.Join(s.modelsDir, m.Dir), s.threads)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.transRec = rec
	s.mu.Unlock()
	return nil
}

// transcribeTranslate is the STT closure for the translate listener: same shape as transcribe, but
// backed by the translate-mode recognizer so the returned text is English.
func (s *service) transcribeTranslate(pcm []byte, f wyoming.AudioFormat, _ string) (string, error) {
	s.mu.RLock()
	rec := s.transRec
	s.mu.RUnlock()
	if rec == nil {
		return "", fmt.Errorf("translate is not loaded")
	}
	text, err := rec.Transcribe(pcm, f.Rate)
	if err != nil {
		return "", err
	}
	if s.cfg.FilterNonSpeech {
		if clean := engine.CleanTranscript(text); clean != text {
			text = clean
		}
	}
	return text, nil
}

// syncTranslateListener opens or closes the translate Wyoming port to match config, mirroring
// syncListeners. It only listens once the recognizer is actually loaded, so a failed load frees the
// port instead of answering with errors.
func (s *service) syncTranslateListener() {
	s.lnMu.Lock()
	defer s.lnMu.Unlock()
	s.mu.RLock()
	rec := s.transRec
	s.mu.RUnlock()

	want := s.translateOn() && rec != nil
	have := s.transLn != nil
	if want == have {
		return
	}
	if !want {
		log.Printf("translate stopped listening on %s", s.transLn.Addr())
		s.transLn.Close() // the accept loop returns on net.ErrClosed
		s.transLn = nil
		return
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", s.cfg.Bind, s.cfg.TranslatePort))
	if err != nil {
		s.setStatus("Translate port %d busy: %v", s.cfg.TranslatePort, err)
		return
	}
	s.transLn = ln
	log.Printf("translate (Whisper → English) listening on %s", ln.Addr())
	go wyoming.ServeSTT(ln, s.transcribeTranslate, s.info, log.Printf)
}

// SwitchTranslate is the UI entry point for turning the translate endpoint on or off.
func (s *service) SwitchTranslate(on bool) error {
	if err := s.useTranslate(on); err != nil {
		s.setStatus("Translate switch failed: %v", err)
		return err
	}
	state := "off"
	if on {
		state = "on"
	}
	s.cfg.Translate = state
	config.Save(s.cfg)
	s.syncTranslateListener()
	s.running()
	return nil
}
