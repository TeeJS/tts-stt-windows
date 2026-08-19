package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/TeeJS/tts-stt-windows/internal/config"
	"github.com/TeeJS/tts-stt-windows/internal/diarize"
	"github.com/TeeJS/tts-stt-windows/internal/models"
)

//go:embed ui.html
var uiHTML []byte

// uiServer hosts the settings page on a loopback port. It's a plain local page rather than a
// native window: the model browser needs search, filtering and progress for 200+ voices, which a
// tray menu can't do, and this adds no GUI toolkit or WebView dependency to the binary.
//
// Bound to 127.0.0.1 on an OS-assigned port. Every mutating route is POST and checks the
// Origin/Sec-Fetch-Site headers, so a page on some other site the user happens to be visiting
// can't drive the settings of this server.
type uiServer struct {
	svc  *service
	port int
	mu   sync.Mutex
}

func startUI(svc *service) (*uiServer, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	u := &uiServer{svc: svc, port: l.Addr().(*net.TCPAddr).Port}
	mux := http.NewServeMux()
	mux.HandleFunc("/", u.handleIndex)
	mux.HandleFunc("/api/state", u.handleState)
	mux.HandleFunc("/api/select", u.guard(u.handleSelect))
	mux.HandleFunc("/api/remove", u.guard(u.handleRemove))
	mux.HandleFunc("/api/settings", u.guard(u.handleSettings))
	mux.HandleFunc("/api/test", u.guard(u.handleTest))
	mux.HandleFunc("/api/meetings", u.guard(u.handleMeetings))
	mux.HandleFunc("/api/speaker-action", u.guard(u.handleSpeakerAction))
	mux.HandleFunc("/api/enroll", u.guard(u.handleUIEnroll))
	go func() {
		if err := http.Serve(l, mux); err != nil {
			log.Printf("settings UI stopped: %v", err)
		}
	}()
	log.Printf("settings UI on http://127.0.0.1:%d", u.port)
	return u, nil
}

func (u *uiServer) URL() string { return fmt.Sprintf("http://127.0.0.1:%d/", u.port) }

// Open launches the settings page in the default browser.
func (u *uiServer) Open() {
	if err := exec.Command("rundll32", "url.dll,FileProtocolHandler", u.URL()).Start(); err != nil {
		log.Printf("could not open settings page: %v", err)
	}
}

// guard rejects cross-site requests: our own page sends Sec-Fetch-Site: same-origin, and anything
// arriving from another site (or with a foreign Origin) is refused.
func (u *uiServer) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != fmt.Sprintf("http://127.0.0.1:%d", u.port) {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		u.mu.Lock() // one settings change at a time; engine swaps are not concurrent-safe
		defer u.mu.Unlock()
		h(w, r)
	}
}

func (u *uiServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(uiHTML)
}

type stateModel struct {
	models.Model
	Installed bool `json:"installed"`
	Active    bool `json:"active"`
}

func (u *uiServer) handleState(w http.ResponseWriter, r *http.Request) {
	svc := u.svc
	all := models.All()
	out := make([]stateModel, 0, len(all)+3)
	// Synthetic entries heading each list. None is in the catalog and none downloads anything:
	// "Off" stops that service entirely, and the built-in Windows voice always exists.
	out = append(out,
		stateModel{
			Model: models.Model{
				ID: offTTSID, Kind: models.TTS, Family: "off", Name: "Off",
				Langs: []string{"multi"}, Notes: "Stop text-to-speech · frees its memory and port",
			},
			Installed: true,
			Active:    isOff(svc.ActiveTTS()),
		},
		stateModel{
			Model: models.Model{
				ID: offSTTID, Kind: models.STT, Family: "off", Name: "Off",
				Langs: []string{"multi"}, Notes: "Stop speech-to-text · frees its memory and port",
			},
			Installed: true,
			Active:    isOff(svc.ActiveSTT()),
		},
		stateModel{
			Model: models.Model{
				ID: sapiVoiceID, Kind: models.TTS, Family: "windows", Name: "Windows built-in",
				Langs: []string{"multi"}, Notes: "Instant · robotic · no download",
			},
			Installed: true,
			Active:    svc.ActiveTTS() == sapiVoiceID,
		})
	for _, m := range all {
		out = append(out, stateModel{
			Model:     m,
			Installed: models.Installed(svc.modelsDir, m),
			Active:    m.ID == svc.ActiveTTS() || m.ID == svc.ActiveSTT(),
		})
	}
	speakers := []diarize.SpeakerDetail{}
	if store, err := diarize.NewEnrollmentStore(enrollmentsDir()); err == nil {
		if d, err := store.ListDetails(); err == nil && d != nil {
			speakers = d
		}
	}
	// Translate is Whisper-only, so the toggle is offered only when the active STT model is Whisper.
	sttM, sttOK := models.ByID(svc.ActiveSTT())
	translateAvailable := sttOK && sttM.Family == "whisper"
	writeJSON(w, map[string]any{
		"status":             svc.Status(),
		"busy":               svc.Busy(),
		"language":           svc.cfg.Language,
		"browseLanguage":     svc.cfg.BrowseLanguage,
		"systemRegion":       systemRegion,
		"speed":              svc.cfg.Speed,
		"setup":              svc.cfg.Setup,
		"filterNonSpeech":    svc.cfg.FilterNonSpeech,
		"activeTTS":          svc.ActiveTTS(),
		"activeSTT":          svc.ActiveSTT(),
		"languages":          models.Languages(),
		"models":             out,
		"ports":              map[string]int{"stt": svc.cfg.STTPort, "tts": svc.cfg.TTSPort, "meetings": svc.cfg.MeetingsPort, "translate": svc.cfg.TranslatePort},
		"meetings":           svc.meetingsOn(),
		"translate":          svc.translateOn(),
		"translateAvailable": translateAvailable,
		"bind":               svc.cfg.Bind,
		"diarThreshold":      svc.cfg.DiarThreshold,
		"speakers":           speakers,
	})
}

// enrollmentsDir is where voice profiles live — one portable .npy per speaker;
// back up or move machines by copying the folder.
func enrollmentsDir() string { return filepath.Join(config.Dir(), "enrollments") }

// handleMeetings turns the meetings service on/off and applies its settings.
func (u *uiServer) handleMeetings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		On        *bool    `json:"on"`
		Threshold *float32 `json:"threshold"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.Threshold != nil && *body.Threshold > 0 && *body.Threshold < 1 {
		u.svc.SetDiarThreshold(*body.Threshold)
	}
	if body.On != nil {
		on := *body.On
		go func() {
			if err := u.svc.SwitchMeetings(on); err != nil {
				log.Printf("meetings switch: %v", err)
			}
		}()
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleSpeakerAction renames or deletes an enrolled voice profile.
func (u *uiServer) handleSpeakerAction(w http.ResponseWriter, r *http.Request) {
	var body struct{ Action, Name, NewName string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	store, err := diarize.NewEnrollmentStore(enrollmentsDir())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	switch body.Action {
	case "rename":
		err = store.Rename(body.Name, body.NewName)
	case "delete":
		err = store.Delete(body.Name)
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleUIEnroll enrolls a speaker from the settings page: a name and a recording,
// no client scripts needed. Requires the meetings service to be on (it owns the
// embedding model).
func (u *uiServer) handleUIEnroll(w http.ResponseWriter, r *http.Request) {
	u.svc.mu.RLock()
	diarSvc := u.svc.diarSvc
	u.svc.mu.RUnlock()
	if diarSvc == nil {
		http.Error(w, "turn on the meetings service first — it loads the voice models enrollment needs", http.StatusConflict)
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		http.Error(w, "bad upload: "+err.Error(), http.StatusBadRequest)
		return
	}
	name := r.FormValue("name")
	if name == "" {
		http.Error(w, "speaker name is required", http.StatusBadRequest)
		return
	}
	f, _, err := r.FormFile("audio")
	if err != nil {
		http.Error(w, "recording file is required", http.StatusBadRequest)
		return
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	samples, err := diarize.DecodeAudio(data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := diarSvc.Enroll(name, samples); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, diarize.ErrBadName) {
			code = http.StatusBadRequest
		}
		http.Error(w, err.Error(), code)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleSelect activates a model, downloading it first if necessary. It returns as soon as the
// work is queued; the page polls /api/state for progress.
func (u *uiServer) handleSelect(w http.ResponseWriter, r *http.Request) {
	var body struct{ ID string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// "off" and "on" are per-service, so the page sends a distinct id for each; the offs
	// map to the same stored value, the ons restore the service's previous model.
	// Everything else is either the built-in voice or a catalog entry.
	id, kind := body.ID, models.TTS
	switch body.ID {
	case offTTSID:
		id = offID
	case offSTTID:
		id, kind = offID, models.STT
	case onTTSID, onSTTID, sapiVoiceID:
	default:
		m, ok := models.ByID(body.ID)
		if !ok {
			http.Error(w, "unknown model", http.StatusNotFound)
			return
		}
		kind = m.Kind
	}
	go func() {
		var err error
		switch {
		case body.ID == onTTSID:
			err = u.svc.TurnOnTTS()
		case body.ID == onSTTID:
			err = u.svc.TurnOnSTT()
		case kind == models.STT:
			err = u.svc.SwitchSTT(id)
		default:
			err = u.svc.SwitchTTS(id)
		}
		if err != nil {
			log.Printf("select %s: %v", body.ID, err)
		}
	}()
	writeJSON(w, map[string]any{"ok": true})
}

// handleRemove deletes a downloaded model. The active model can't be removed — that would leave
// the server with nothing to answer with.
func (u *uiServer) handleRemove(w http.ResponseWriter, r *http.Request) {
	var body struct{ ID string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.ID == u.svc.ActiveTTS() || body.ID == u.svc.ActiveSTT() {
		http.Error(w, "that model is in use — pick another one first", http.StatusConflict)
		return
	}
	m, ok := models.ByID(body.ID)
	if !ok {
		http.Error(w, "unknown model", http.StatusNotFound)
		return
	}
	if err := models.Remove(u.svc.modelsDir, m); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleSettings applies the language and speaking-rate controls.
func (u *uiServer) handleSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Language        string   `json:"language"`
		BrowseLanguage  *string  `json:"browseLanguage"` // pointer: "" is a real value (show all languages)
		Voice           string   `json:"voice"`          // chosen in the setup card; "" = pick a default
		Speed           *float32 `json:"speed"`
		Setup           *bool    `json:"setup"`
		FilterNonSpeech *bool    `json:"filterNonSpeech"`
		Translate       *bool    `json:"translate"` // enable/disable the Whisper→English translate endpoint
		// Which services the setup card enables. Absent (older page) = the
		// pre-choice behavior: voice and dictation on, transcription off.
		Services *struct {
			TTS      bool `json:"tts"`
			STT      bool `json:"stt"`
			Meetings bool `json:"meetings"`
		} `json:"services"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.Language != "" {
		u.svc.SetLanguage(body.Language)
	}
	if body.Speed != nil {
		u.svc.SetSpeed(*body.Speed)
	}
	if body.BrowseLanguage != nil {
		u.svc.SetBrowseLanguage(*body.BrowseLanguage)
	}
	if body.FilterNonSpeech != nil {
		u.svc.SetFilterNonSpeech(*body.FilterNonSpeech)
	}
	if body.Translate != nil {
		// Errors (e.g. the active model isn't Whisper) surface on the status line; the page re-reads
		// state and reflects the real on/off, so a failed enable simply stays off.
		_ = u.svc.SwitchTranslate(*body.Translate)
	}
	// Answering the language question releases the service to pick and download models for the
	// language the user actually speaks — on first run, and again whenever they re-run it.
	if body.Setup != nil && *body.Setup {
		tts, stt, meetings := true, true, false
		if body.Services != nil {
			tts, stt, meetings = body.Services.TTS, body.Services.STT, body.Services.Meetings
		}
		u.svc.CompleteSetup(body.Voice, tts, stt, meetings)
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleTest speaks a phrase through the active voice so the user can hear it before committing.
func (u *uiServer) handleTest(w http.ResponseWriter, r *http.Request) {
	var body struct{ Text string }
	json.NewDecoder(r.Body).Decode(&body)
	if body.Text == "" {
		body.Text = "This is how I sound."
	}
	pcm, format, err := u.svc.synthesize(body.Text)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(pcm)+44))
	w.Write(wavHeader(format.Rate, format.Channels, len(pcm)))
	w.Write(pcm)
}

// wavHeader builds a 44-byte PCM header for a known-length body (the test clip is fully
// synthesized before it's sent, unlike the streamed Wyoming path).
func wavHeader(rate, channels, dataLen int) []byte {
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	putU32(h[4:], uint32(36+dataLen))
	copy(h[8:], "WAVEfmt ")
	putU32(h[16:], 16)
	putU16(h[20:], 1)
	putU16(h[22:], uint16(channels))
	putU32(h[24:], uint32(rate))
	putU32(h[28:], uint32(rate*channels*2))
	putU16(h[32:], uint16(channels*2))
	putU16(h[34:], 16)
	copy(h[36:], "data")
	putU32(h[40:], uint32(dataLen))
	return h
}

func putU16(b []byte, v uint16) { b[0], b[1] = byte(v), byte(v>>8) }
func putU32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}
