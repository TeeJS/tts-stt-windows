// Package voices stores the reference clips PocketTTS clones from: one WAV per voice under
// %APPDATA%\tts-sst\voices, the filename (minus .wav) being the voice's name. A voice is selected
// with the id "pocket:<name>" (see IDPrefix), the same one-string convention as catalog voices
// and the Windows built-in voice.
//
// Clips are normalized on Save to 16 kHz mono PCM16 WAV regardless of what was uploaded, so
// Load never has to decode MP3 or odd WAV layouts, and a folder copy moves the voices intact.
package voices

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// IDPrefix marks a cloned voice in cfg.TTSVoice: "pocket:<name>".
	IDPrefix = "pocket:"
	// SampleRate is the rate every stored clip is written at (what diarize.DecodeAudio emits).
	SampleRate = 16000
	// MinSeconds / MaxSeconds bound a reference clip. Under ~3 s the embedding is unreliable;
	// over ~30 s only adds pauses (which carry into the output) and embedding time.
	MinSeconds = 3.0
	MaxSeconds = 30.0
)

var (
	ErrBadName  = errors.New("invalid voice name")
	ErrNotFound = errors.New("voice not found")
	ErrExists   = errors.New("voice already exists")
	ErrTooShort = errors.New("clip too short")
	ErrTooLong  = errors.New("clip too long")
)

type Store struct {
	dir string
}

// writeMu serializes Save/Rename/Delete across Store instances (the UI makes one per request),
// so two uploads under the same name can't both pass the exists check.
var writeMu sync.Mutex

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	return &Store{dir: abs}, nil
}

// ID returns the config id for a voice name; Name is its inverse (ok=false for other ids).
func ID(name string) string { return IDPrefix + name }
func Name(id string) (string, bool) {
	if !strings.HasPrefix(id, IDPrefix) {
		return "", false
	}
	return strings.TrimPrefix(id, IDPrefix), true
}

// pathFor validates a name arriving straight from an HTTP form field (same checks as the
// diarization enrollment store) and maps it to its clip path.
func (s *Store) pathFor(name string) (string, error) {
	name = strings.TrimSpace(name)
	// Path separators plus the characters NTFS refuses in a file name.
	if name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, `/\:<>"|?*`) || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("%w: %q", ErrBadName, name)
	}
	p := filepath.Join(s.dir, name+".wav")
	if filepath.Dir(p) != s.dir {
		return "", fmt.Errorf("%w: %q", ErrBadName, name)
	}
	return p, nil
}

// Path returns the clip file for a voice, or ErrNotFound.
func (s *Store) Path(name string) (string, error) {
	p, err := s.pathFor(name)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return p, nil
}

// Exists reports whether a clip is stored under name (false for invalid names too).
func (s *Store) Exists(name string) bool {
	_, err := s.Path(name)
	return err == nil
}

// Save writes a clip as 16 kHz mono PCM16 WAV. samples must already be at SampleRate (what
// diarize.DecodeAudio returns). Length is checked here, after decoding, so the caller is
// responsible for capping the upload size before it decodes.
func (s *Store) Save(name string, samples []float32) error {
	p, err := s.pathFor(name)
	if err != nil {
		return err
	}
	writeMu.Lock()
	defer writeMu.Unlock()
	secs := float64(len(samples)) / SampleRate
	if secs < MinSeconds {
		return fmt.Errorf("%w: %.1fs (need at least %.0fs)", ErrTooShort, secs, MinSeconds)
	}
	if secs > MaxSeconds {
		return fmt.Errorf("%w: %.1fs (at most %.0fs)", ErrTooLong, secs, MaxSeconds)
	}
	// Never overwrite: an upload under a taken name is a mistake, not a replacement. Compared
	// case-insensitively because Windows file names are, so "Bob" and "bob" are one clip.
	existing, _ := s.List()
	for _, n := range existing {
		if strings.EqualFold(n, strings.TrimSpace(name)) {
			return fmt.Errorf("%w: %q", ErrExists, n)
		}
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, encodeWAV(samples, SampleRate), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Load returns a clip's samples and rate.
func (s *Store) Load(name string) ([]float32, int, error) {
	p, err := s.Path(name)
	if err != nil {
		return nil, 0, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, 0, err
	}
	return decodeWAV(data)
}

func (s *Store) Delete(name string) error {
	p, err := s.pathFor(name)
	if err != nil {
		return err
	}
	writeMu.Lock()
	defer writeMu.Unlock()
	if err := os.Remove(p); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %q", ErrNotFound, name)
		}
		return err
	}
	return nil
}

// Rename keeps the clip file (and so its mtime).
func (s *Store) Rename(from, to string) error {
	src, err := s.pathFor(from)
	if err != nil {
		return err
	}
	dst, err := s.pathFor(to)
	if err != nil {
		return err
	}
	writeMu.Lock()
	defer writeMu.Unlock()
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return fmt.Errorf("%w: %q", ErrNotFound, from)
	}
	if !strings.EqualFold(src, dst) { // a case-only rename of the same file is allowed
		if _, err := os.Stat(dst); err == nil {
			return fmt.Errorf("%w: %q", ErrExists, to)
		}
	}
	return os.Rename(src, dst)
}

// List returns voice names, sorted.
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".wav") {
			names = append(names, strings.TrimSuffix(e.Name(), ".wav"))
		}
	}
	sort.Strings(names)
	return names, nil
}

type Detail struct {
	Name    string  `json:"name"`
	ID      string  `json:"id"`      // "pocket:<name>", what the voice picker sends
	Seconds float64 `json:"seconds"` // clip length
	AddedAt string  `json:"added_at"` // RFC3339 UTC, from the file's mtime
}

func (s *Store) ListDetails() ([]Detail, error) {
	names, err := s.List()
	if err != nil {
		return nil, err
	}
	details := make([]Detail, 0, len(names))
	for _, n := range names {
		d := Detail{Name: n, ID: ID(n)}
		if fi, err := os.Stat(filepath.Join(s.dir, n+".wav")); err == nil {
			d.AddedAt = fi.ModTime().UTC().Format(time.RFC3339)
			if fi.Size() > 44 {
				d.Seconds = float64(fi.Size()-44) / (SampleRate * 2)
			}
		}
		details = append(details, d)
	}
	return details, nil
}

// ---- minimal PCM16 mono WAV, the one layout this package ever writes ----

func encodeWAV(samples []float32, rate int) []byte {
	n := len(samples) * 2
	out := make([]byte, 44+n)
	copy(out[0:], "RIFF")
	binary.LittleEndian.PutUint32(out[4:], uint32(36+n))
	copy(out[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(out[16:], 16)
	binary.LittleEndian.PutUint16(out[20:], 1) // PCM
	binary.LittleEndian.PutUint16(out[22:], 1) // mono
	binary.LittleEndian.PutUint32(out[24:], uint32(rate))
	binary.LittleEndian.PutUint32(out[28:], uint32(rate*2))
	binary.LittleEndian.PutUint16(out[32:], 2)
	binary.LittleEndian.PutUint16(out[34:], 16)
	copy(out[36:], "data")
	binary.LittleEndian.PutUint32(out[40:], uint32(n))
	for i, s := range samples {
		if s > 1 {
			s = 1
		} else if s < -1 {
			s = -1
		}
		binary.LittleEndian.PutUint16(out[44+i*2:], uint16(int16(s*32767)))
	}
	return out
}

// decodeWAV reads back exactly what encodeWAV writes (plus any extra chunks a user-edited
// file might carry): PCM16 mono.
func decodeWAV(data []byte) ([]float32, int, error) {
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, 0, errors.New("not a WAV file")
	}
	var rate, channels, bits int
	pos := 12
	for pos+8 <= len(data) {
		id := string(data[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(data[pos+4:]))
		body := pos + 8
		if body+size > len(data) {
			size = len(data) - body
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return nil, 0, errors.New("wav: short fmt chunk")
			}
			channels = int(binary.LittleEndian.Uint16(data[body+2:]))
			rate = int(binary.LittleEndian.Uint32(data[body+4:]))
			bits = int(binary.LittleEndian.Uint16(data[body+14:]))
		case "data":
			if rate == 0 || channels != 1 || bits != 16 {
				return nil, 0, fmt.Errorf("wav: expected 16-bit mono, got %d-bit %d-channel", bits, channels)
			}
			n := size / 2
			out := make([]float32, n)
			for i := 0; i < n; i++ {
				out[i] = float32(int16(binary.LittleEndian.Uint16(data[body+i*2:]))) / 32768
			}
			return out, rate, nil
		}
		pos = body + size + size%2
	}
	return nil, 0, errors.New("wav: no data chunk")
}
