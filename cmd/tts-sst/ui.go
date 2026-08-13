package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"sync"

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
	out := make([]stateModel, 0, len(all)+1)
	// The built-in Windows voice heads the list: always present, nothing to download.
	out = append(out, stateModel{
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
	writeJSON(w, map[string]any{
		"status":    svc.Status(),
		"busy":      svc.Busy(),
		"language":  svc.cfg.Language,
		"speed":     svc.cfg.Speed,
		"setup":     svc.cfg.Setup,
		"activeTTS": svc.ActiveTTS(),
		"activeSTT": svc.ActiveSTT(),
		"languages": models.Languages(),
		"models":    out,
		"ports":     map[string]int{"stt": svc.cfg.STTPort, "tts": svc.cfg.TTSPort},
	})
}

// handleSelect activates a model, downloading it first if necessary. It returns as soon as the
// work is queued; the page polls /api/state for progress.
func (u *uiServer) handleSelect(w http.ResponseWriter, r *http.Request) {
	var body struct{ ID string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	kind := models.TTS
	if body.ID != sapiVoiceID {
		m, ok := models.ByID(body.ID)
		if !ok {
			http.Error(w, "unknown model", http.StatusNotFound)
			return
		}
		kind = m.Kind
	}
	go func() {
		var err error
		if kind == models.STT {
			err = u.svc.SwitchSTT(body.ID)
		} else {
			err = u.svc.SwitchTTS(body.ID)
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
		Language string   `json:"language"`
		Speed    *float32 `json:"speed"`
		Setup    *bool    `json:"setup"`
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
	// Answering the first-run question is what releases the service to pick and download models —
	// now for the language the user actually speaks.
	if body.Setup != nil && *body.Setup && !u.svc.cfg.Setup {
		u.svc.CompleteSetup()
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
