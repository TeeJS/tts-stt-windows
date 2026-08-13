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
	STTModel string `json:"sttModel"` // registry name (models.Registry), "" = default
	TTSVoice string `json:"ttsVoice"` // registry name, "" = default
	Language string `json:"language"`
	Threads  int    `json:"threads"` // 0 = auto
}

func Defaults() Config {
	return Config{Bind: "127.0.0.1", STTPort: 10300, TTSPort: 10200, Language: "en"}
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
	return c
}

func Save(c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path(), b, 0o644)
}
