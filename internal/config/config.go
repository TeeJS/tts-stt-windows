// Package config persists user settings at %APPDATA%\tts-sst\config.json. Missing file or
// fields fall back to defaults — the app must run correctly with no config at all.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type Config struct {
	Bind     string `json:"bind"`
	STTPort  int    `json:"sttPort"`
	TTSPort  int    `json:"ttsPort"`
	STTModel string `json:"sttModel"` // catalog id, "" = pick a default for Language
	TTSVoice string `json:"ttsVoice"` // catalog id or "windows-builtin", "" = pick a default
	Language string `json:"language"` // the user's language: drives defaults and multi-language hints
	// BrowseLanguage is the language the model lists in the settings page are filtered to.
	// Deliberately separate from Language: filtering the list to audition German voices must not
	// silently change the language hint handed to speech recognition. "" follows Language.
	BrowseLanguage string  `json:"browseLanguage"`
	Speed          float32 `json:"speed"`   // voice speaking rate, 1.0 = natural
	Threads        int     `json:"threads"` // 0 = auto
	Setup          bool    `json:"setup"`   // true once the first-run language choice has been made
	// FilterNonSpeech drops transcripts that are only a model's description of a sound —
	// "(clicking)", "[BLANK_AUDIO]" — instead of passing them on as if they were spoken.
	FilterNonSpeech bool `json:"filterNonSpeech"`

	// Meetings switches the meeting-diarization HTTP service: "on", or "" / "off" for off.
	// Unlike STT/TTS this is not a model id — the service needs several models at once, so
	// on/off and the model choices are separate fields.
	Meetings       string `json:"meetings"`
	MeetingsPort   int    `json:"meetingsPort"`
	MeetingsSTT    string `json:"meetingsSTT"`    // catalog id; "" = share the active STT when usable, else the default STT model
	DiarSegModel   string `json:"diarSegModel"`   // catalog id; "" = default segmentation model
	DiarEmbedModel string `json:"diarEmbedModel"` // catalog id; "" = default speaker-embedding model
	// DiarThreshold is the speaker-identification cosine cutoff (per-request override allowed).
	// DiarClusterThreshold tunes sherpa's clustering — a different knob, config-level only.
	DiarThreshold        float32 `json:"diarThreshold"`
	DiarClusterThreshold float32 `json:"diarClusterThreshold"`
}

func Defaults() Config {
	return Config{Bind: "127.0.0.1", STTPort: 10300, TTSPort: 10200, Language: "en", Speed: 1.0, FilterNonSpeech: true,
		MeetingsPort: 10301, DiarThreshold: 0.70, DiarClusterThreshold: 0.5}
}

// Dir is the app's data directory (%APPDATA%\tts-sst), created on demand.
func Dir() string {
	base := os.Getenv("APPDATA")
	if base == "" {
		base = "."
	}
	d := filepath.Join(base, "tts-sst")
	os.MkdirAll(d, 0o755)
	return d
}

func path() string { return filepath.Join(Dir(), "config.json") }

// Load returns saved settings over defaults. A corrupt file is ignored (defaults win) rather
// than blocking startup — the tray is a background service, not something to error-loop.
func Load() Config {
	c := Defaults()
	b, err := os.ReadFile(path())
	if err != nil {
		return c
	}
	_ = json.Unmarshal(b, &c)
	if c.Bind == "" {
		c.Bind = "127.0.0.1"
	}
	if c.STTPort <= 0 {
		c.STTPort = 10300
	}
	if c.TTSPort <= 0 {
		c.TTSPort = 10200
	}
	if c.Language == "" {
		c.Language = "en"
	}
	if c.Speed <= 0 {
		c.Speed = 1.0
	}
	if c.MeetingsPort <= 0 {
		c.MeetingsPort = 10301
	}
	if c.DiarThreshold <= 0 {
		c.DiarThreshold = 0.70
	}
	if c.DiarClusterThreshold <= 0 {
		c.DiarClusterThreshold = 0.5
	}
	return c
}

// Exists reports whether a config file has been written yet — the signal for "first run".
func Exists() bool {
	_, err := os.Stat(path())
	return err == nil
}

func Save(c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path(), b, 0o644)
}
