package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/TeeJS/tts-stt-windows/internal/config"
	"github.com/TeeJS/tts-stt-windows/internal/diarize"
	"github.com/TeeJS/tts-stt-windows/internal/engine"
	"github.com/TeeJS/tts-stt-windows/internal/models"
)

// The meetings service: batch speaker diarization + identification + transcription
// over HTTP, wire-compatible with the Python meeting-diarizer on port 10301. Unlike
// STT/TTS it is off by default and switched on from the settings page.

const (
	defaultDiarSeg   = "pyannote-segmentation-3-0"
	defaultDiarEmbed = "eres2net-en-voxceleb"
)

// meetingsOn reports whether the meetings service is enabled in config.
func (s *service) meetingsOn() bool { return s.cfg.Meetings == "on" }

// sharedSTT lets the meetings pipeline use whatever STT engine is currently live,
// so the common case (meetings model == dictation model) costs no extra memory.
// Fetches the engine per call, so a model swap mid-meeting just means later
// windows transcribe with the new engine.
type sharedSTT struct{ s *service }

func (sh sharedSTT) TranscribeTokens(samples []float32, rate int) (string, []engine.Token) {
	sh.s.mu.RLock()
	rec := sh.s.stt
	sh.s.mu.RUnlock()
	if rec == nil {
		return "", nil
	}
	return rec.TranscribeTokens(samples, rate)
}

// diarModelPath locates the actual .onnx file inside an installed diar model dir:
// model.onnx for archives (preferring full precision over the bundled int8), the
// download's own filename for bare-.onnx installs, else whatever single .onnx exists.
func diarModelPath(root string, m models.Model) (string, error) {
	dir := filepath.Join(root, m.Dir)
	for _, name := range []string{"model.onnx", filepath.Base(m.URL)} {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*.onnx"))
	if len(matches) > 0 {
		return matches[0], nil
	}
	return "", fmt.Errorf("no .onnx file in %s", dir)
}

// useMeetings loads or unloads the meetings service's models. On enable it
// downloads anything missing, builds the diarizer, and picks the transcriber:
// the live STT engine when the meetings model matches it, otherwise a dedicated
// recognizer instance.
func (s *service) useMeetings(on bool) error {
	if !on {
		s.mu.Lock()
		diar := s.diar
		rec := s.diarRec
		s.diar, s.diarSvc, s.diarRec = nil, nil, nil
		s.mu.Unlock()
		if diar != nil {
			go diar.Close()
		}
		closeAsync(rec)
		return nil
	}

	segM, ok := models.ByID(s.resolve(s.cfg.DiarSegModel, models.Diar, defaultDiarSeg))
	if !ok {
		return errors.New("segmentation model missing from catalog")
	}
	embedM, ok := models.ByID(s.resolve(s.cfg.DiarEmbedModel, models.Diar, defaultDiarEmbed))
	if !ok {
		return errors.New("embedding model missing from catalog")
	}
	// The transcription model: the configured one, the active STT, or the default
	// for the user's language, in that order.
	sttID := s.cfg.MeetingsSTT
	if sttID == "" {
		if active := s.ActiveSTT(); active != "" && !isOff(active) {
			sttID = active
		} else {
			_, def := models.DefaultsFor(s.cfg.Language)
			sttID = def.ID
		}
	}
	sttM, ok := models.ByID(sttID)
	if !ok || sttM.Kind != models.STT {
		return fmt.Errorf("meetings speech model %q not in catalog", sttID)
	}

	for _, m := range []models.Model{segM, embedM, sttM} {
		if err := s.ensure(m); err != nil {
			return err
		}
	}
	segPath, err := diarModelPath(s.modelsDir, segM)
	if err != nil {
		return err
	}
	embedPath, err := diarModelPath(s.modelsDir, embedM)
	if err != nil {
		return err
	}

	s.setBusy("Loading meeting-diarization models…")
	defer s.setBusy("")

	store, err := diarize.NewEnrollmentStore(enrollmentsDir())
	if err != nil {
		return err
	}
	diar, err := diarize.NewDiarizer(segPath, embedPath, s.threads, s.cfg.DiarClusterThreshold, store)
	if err != nil {
		return err
	}

	var rec diarize.Transcriber
	var dedicated *engine.Recognizer
	if sttID == s.ActiveSTT() {
		rec = sharedSTT{s}
	} else {
		dedicated, err = engine.NewSTT(filepath.Join(s.modelsDir, sttM.Dir), sttM.Family, sttLanguage(sttM, s.cfg.Language), s.threads)
		if err != nil {
			diar.Close()
			return err
		}
		rec = dedicated
	}

	svc := diarize.NewService(diar, rec, store, float64(s.cfg.DiarThreshold), s.setBusy)

	s.mu.Lock()
	oldDiar, oldRec := s.diar, s.diarRec
	s.diar, s.diarSvc, s.diarRec = diar, svc, dedicated
	s.mu.Unlock()
	if oldDiar != nil {
		go oldDiar.Close()
	}
	closeAsync(oldRec)
	return nil
}

// syncMeetingsListener opens or closes the meetings HTTP port to match config,
// mirroring syncListeners for the Wyoming services.
func (s *service) syncMeetingsListener() {
	s.lnMu.Lock()
	defer s.lnMu.Unlock()

	want := s.meetingsOn()
	have := s.meetLn != nil
	if want == have {
		return
	}
	if !want {
		log.Printf("meetings stopped listening on %s", s.meetLn.Addr())
		s.meetLn.Close()
		s.meetLn = nil
		return
	}
	s.mu.RLock()
	svc := s.diarSvc
	s.mu.RUnlock()
	if svc == nil {
		return // models failed to load; useMeetings already reported it
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", s.cfg.Bind, s.cfg.MeetingsPort))
	if err != nil {
		s.setStatus("Meetings port %d busy: %v", s.cfg.MeetingsPort, err)
		return
	}
	s.meetLn = ln
	log.Printf("meetings listening on %s", ln.Addr())
	srv := &http.Server{
		Handler:           svc.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: /transcribe blocks for the length of the job and
		// clients wait up to an hour by contract.
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("meetings server: %v", err)
		}
	}()
}

// SwitchMeetings is the UI entry point for turning the meetings service on or off.
func (s *service) SwitchMeetings(on bool) error {
	state := "off"
	if on {
		state = "on"
	}
	if err := s.useMeetings(on); err != nil {
		s.setStatus("Meetings switch failed: %v", err)
		return err
	}
	s.cfg.Meetings = state
	config.Save(s.cfg)
	s.syncMeetingsListener()
	s.running()
	return nil
}

// SetDiarThreshold persists the identification threshold and applies it to the
// running service on next request (the service reads it per-request via config).
func (s *service) SetDiarThreshold(t float32) {
	s.cfg.DiarThreshold = t
	config.Save(s.cfg)
	// Rebuild the service default without reloading models.
	s.mu.Lock()
	if s.diar != nil && s.diarSvc != nil {
		s.diarSvc.SetDefaultThreshold(float64(t))
	}
	s.mu.Unlock()
}
