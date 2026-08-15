package diarize

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Service is the meeting-diarization HTTP service, wire-compatible with the Python
// meeting-diarizer's FastAPI app: multipart POST /transcribe and /identify and
// /enroll, speaker management, /health. Existing clients work unchanged against it.
//
// Requests block until processing finishes (clients POST and wait, up to an hour);
// one mutex serializes jobs the way the Python service's single worker did.
type Service struct {
	diar             *Diarizer
	rec              Transcriber
	store            *EnrollmentStore
	defaultThreshold float64
	progress         func(string) // busy-status sink (tray tooltip); may be nil

	mu sync.Mutex // one meeting job at a time
}

func NewService(diar *Diarizer, rec Transcriber, store *EnrollmentStore, defaultThreshold float64, progress func(string)) *Service {
	if progress == nil {
		progress = func(string) {}
	}
	return &Service{diar: diar, rec: rec, store: store, defaultThreshold: defaultThreshold, progress: progress}
}

// SetDefaultThreshold updates the identification threshold used when a request
// doesn't carry one. Safe while jobs run — the next request picks it up.
func (s *Service) SetDefaultThreshold(t float64) {
	s.mu.Lock()
	s.defaultThreshold = t
	s.mu.Unlock()
}

// Handler returns the HTTP mux implementing the wire contract.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /transcribe", s.handleTranscribe)
	mux.HandleFunc("POST /identify", s.handleIdentify)
	mux.HandleFunc("POST /enroll", s.handleEnroll)
	mux.HandleFunc("GET /speakers", s.handleSpeakers)
	mux.HandleFunc("POST /speakers/{name}/rename", s.handleRename)
	mux.HandleFunc("DELETE /speakers/{name}", s.handleDelete)
	return mux
}

// writeJSON marshals v preserving struct field order (our structs mirror the
// Python service's key order).
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// httpError mirrors FastAPI's {"detail": message} error body.
func httpError(w http.ResponseWriter, code int, format string, args ...any) {
	writeJSON(w, code, map[string]string{"detail": fmt.Sprintf(format, args...)})
}

// audioRequest parses the multipart form shared by /transcribe and /identify:
// `audio` (file, required), `threshold` (optional float), `attendees`
// (optional comma-separated list). name is the upload's original filename.
func (s *Service) audioRequest(r *http.Request) (samples []float32, threshold float64, attendees []string, name string, err error) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		return nil, 0, nil, "", fmt.Errorf("bad multipart body: %w", err)
	}
	f, hdr, err := r.FormFile("audio")
	if err != nil {
		return nil, 0, nil, "", errors.New("missing 'audio' file field")
	}
	name = hdr.Filename
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, 0, nil, "", fmt.Errorf("reading upload: %w", err)
	}
	samples, err = DecodeAudio(data)
	if err != nil {
		return nil, 0, nil, "", err
	}

	threshold = s.defaultThreshold
	if v := r.FormValue("threshold"); v != "" {
		t, perr := strconv.ParseFloat(v, 64)
		if perr != nil {
			return nil, 0, nil, "", fmt.Errorf("bad threshold %q", v)
		}
		threshold = t
	}
	for _, a := range strings.Split(r.FormValue("attendees"), ",") {
		if a = strings.TrimSpace(a); a != "" {
			attendees = append(attendees, a)
		}
	}
	return samples, threshold, attendees, name, nil
}

// responseFilename suggests "<upload basename>-diarizer-response.json" — the
// naming convention downstream pipelines already use when saving these results.
func responseFilename(upload string) string {
	base := strings.TrimSuffix(filepath.Base(upload), filepath.Ext(upload))
	base = strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r < 0x20 {
			return -1
		}
		return r
	}, base)
	if base == "" || base == "." {
		base = "meeting"
	}
	return base + "-diarizer-response.json"
}

func (s *Service) handleTranscribe(w http.ResponseWriter, r *http.Request) {
	samples, threshold, attendees, upload, err := s.audioRequest(r)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	minutes := float64(len(samples)) / pipelineRate / 60
	s.progress(fmt.Sprintf("Transcribing meeting (%.0f min)…", minutes))
	defer s.progress("")

	segments, report := s.diar.Transcribe(s.rec, samples, threshold, attendees)
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", responseFilename(upload)))
	writeJSON(w, http.StatusOK, struct {
		SpeakerReport *SpeakerReport `json:"speaker_report"`
		Segments      []Segment      `json:"segments"`
	}{report, segments})
}

func (s *Service) handleIdentify(w http.ResponseWriter, r *http.Request) {
	samples, threshold, attendees, _, err := s.audioRequest(r)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.progress("Identifying speakers…")
	defer s.progress("")

	// /identify returns the bare report object, unwrapped — clients rely on this.
	writeJSON(w, http.StatusOK, s.diar.IdentifySpeakers(samples, threshold, attendees))
}

func (s *Service) handleEnroll(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		httpError(w, http.StatusBadRequest, "bad multipart body: %v", err)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		httpError(w, http.StatusBadRequest, "missing 'name' field")
		return
	}
	f, _, err := r.FormFile("audio")
	if err != nil {
		httpError(w, http.StatusBadRequest, "missing 'audio' file field")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		httpError(w, http.StatusBadRequest, "reading upload: %v", err)
		return
	}
	samples, err := DecodeAudio(data)
	if err != nil {
		s.badRequest(w, err)
		return
	}

	if err := s.Enroll(name, samples); err != nil {
		if errors.Is(err, ErrBadName) {
			httpError(w, http.StatusBadRequest, "%v", err)
		} else {
			httpError(w, http.StatusInternalServerError, "%v", err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "enrolled", "name": name})
}

// Enroll computes and stores a voice profile, serialized against running jobs.
// Shared by the HTTP /enroll endpoint and the settings page's enrollment form.
func (s *Service) Enroll(name string, samples []float32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.progress(fmt.Sprintf("Enrolling %s…", name))
	defer s.progress("")
	if err := s.diar.EnrollSpeaker(name, samples); err != nil {
		return err
	}
	log.Printf("meetings: enrolled speaker %q", name)
	return nil
}

func (s *Service) handleSpeakers(w http.ResponseWriter, r *http.Request) {
	names, err := s.store.ListSpeakers()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	details, err := s.store.ListDetails()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if names == nil {
		names = []string{}
	}
	if details == nil {
		details = []SpeakerDetail{}
	}
	writeJSON(w, http.StatusOK, struct {
		Speakers []string        `json:"speakers"`
		Details  []SpeakerDetail `json:"details"`
	}{names, details})
}

func (s *Service) handleRename(w http.ResponseWriter, r *http.Request) {
	from := r.PathValue("name")
	if err := r.ParseMultipartForm(1 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		httpError(w, http.StatusBadRequest, "bad form: %v", err)
		return
	}
	to := strings.TrimSpace(r.FormValue("new_name"))
	if to == "" {
		httpError(w, http.StatusBadRequest, "missing 'new_name' field")
		return
	}
	switch err := s.store.Rename(from, to); {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]string{"status": "renamed", "from": from, "to": to})
	case errors.Is(err, ErrNotFound):
		httpError(w, http.StatusNotFound, "speaker %q not found", from)
	case errors.Is(err, ErrExists):
		httpError(w, http.StatusConflict, "speaker %q already exists", to)
	default:
		httpError(w, http.StatusBadRequest, "%v", err)
	}
}

func (s *Service) handleDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	switch err := s.store.Delete(name); {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "name": name})
	case errors.Is(err, ErrNotFound):
		httpError(w, http.StatusNotFound, "speaker %q not found", name)
	default:
		httpError(w, http.StatusBadRequest, "%v", err)
	}
}

// badRequest maps decode/validation failures to 400 and everything else to 500,
// mirroring the Python service's status usage.
func (s *Service) badRequest(w http.ResponseWriter, err error) {
	var uerr *UnsupportedAudioError
	if errors.As(err, &uerr) {
		httpError(w, http.StatusBadRequest, "%v", err)
		return
	}
	httpError(w, http.StatusBadRequest, "%v", err)
}
