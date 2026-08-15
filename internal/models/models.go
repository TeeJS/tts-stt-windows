// Package models manages the on-disk model store (%APPDATA%\tts-sst\models) and the catalog of
// everything installable: 200+ Piper/Coqui voices and 30+ speech-recognition models across 50+
// languages, generated from the sherpa-onnx releases by tools/gen-catalog and embedded here so
// browsing and filtering work offline. Only downloading needs the network.
package models

import (
	"archive/tar"
	"compress/bzip2"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

//go:embed catalog.json
var catalogJSON []byte

type Kind string

const (
	STT  Kind = "stt"
	TTS  Kind = "tts"
	Diar Kind = "diar" // speaker diarization / embedding models for the meetings service
)

// Model is one installable voice or speech-recognition model.
type Model struct {
	ID      string   `json:"id"`
	Kind    Kind     `json:"kind"`
	Family  string   `json:"family"`  // piper, coqui, whisper, parakeet, sense-voice, moonshine, dolphin
	Name    string   `json:"name"`    // human label ("Alan", "Whisper small")
	Langs   []string `json:"langs"`   // primary language codes, or ["multi"]
	Region  string   `json:"region"`  // display-only accent/region ("GB"), never used for filtering
	Quality string   `json:"quality"` // voices: x_low | low | medium | high
	Size    int64    `json:"size"`    // archive bytes, verified after download
	URL     string   `json:"url"`
	Dir     string   `json:"dir"` // directory the archive extracts to
	Notes   string   `json:"notes"`
}

type catalog struct {
	Languages map[string]string `json:"languages"` // code -> display name, for the filter dropdown
	Entries   []Model           `json:"entries"`
}

var loaded catalog

func init() {
	if err := json.Unmarshal(catalogJSON, &loaded); err != nil {
		// The catalog is generated and embedded at build time; a parse failure is a build bug,
		// not a runtime condition, so fail loudly rather than starting with an empty list.
		panic("models: embedded catalog is invalid: " + err.Error())
	}
}

// All returns every catalog entry, voices and speech models alike.
func All() []Model { return loaded.Entries }

// Languages maps each language code present in the catalog to its display name.
func Languages() map[string]string { return loaded.Languages }

// ByID looks up one entry.
func ByID(id string) (Model, bool) {
	for _, m := range loaded.Entries {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

// OfKind returns entries of one kind, sorted by language then name — the order the settings list
// renders in.
func OfKind(k Kind) []Model {
	var out []Model
	for _, m := range loaded.Entries {
		if m.Kind == k {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		li, lj := strings.Join(out[i].Langs, ","), strings.Join(out[j].Langs, ",")
		if li != lj {
			return li < lj
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// ForLanguage returns entries usable by a speaker of `lang` — that language plus multi-language
// models, which serve every language.
func ForLanguage(k Kind, lang string) []Model {
	var out []Model
	for _, m := range OfKind(k) {
		for _, l := range m.Langs {
			if l == lang || l == "multi" {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

// DefaultsFor picks a sensible starting voice and speech model for a language: the smallest
// multi-language speech model that covers it (or the smallest model in that language), and a
// medium-quality voice if one exists. Returns zero values when a language has no voice at all.
func DefaultsFor(lang string) (voice Model, stt Model) {
	best := func(ms []Model, prefer func(Model) bool) Model {
		var pick Model
		for _, m := range ms {
			switch {
			case pick.ID == "":
				pick = m
			case prefer(m) && !prefer(pick):
				pick = m
			case prefer(m) == prefer(pick) && m.Size < pick.Size:
				pick = m
			}
		}
		return pick
	}
	// Voices: prefer medium quality — low is noticeably worse, high costs far more CPU per word.
	voice = best(ForLanguage(TTS, lang), func(m Model) bool { return m.Quality == "medium" })
	// Speech: prefer Parakeet v3 where it applies (fastest good multilingual model, and Handy's
	// default for the same reason); otherwise the smallest model covering the language.
	stt = best(ForLanguage(STT, lang), func(m Model) bool { return m.ID == "parakeet-tdt-0.6b-v3" })
	return voice, stt
}

// Installed reports whether the model's directory exists in the store.
func Installed(root string, m Model) bool {
	fi, err := os.Stat(filepath.Join(root, m.Dir))
	return err == nil && fi.IsDir()
}

// InstalledIDs returns the ids of every catalog entry present on disk.
func InstalledIDs(root string) []string {
	var out []string
	for _, m := range loaded.Entries {
		if Installed(root, m) {
			out = append(out, m.ID)
		}
	}
	return out
}

// Remove deletes an installed model's directory.
func Remove(root string, m Model) error {
	return os.RemoveAll(filepath.Join(root, m.Dir))
}

// Progress receives download state: bytes done of total for the named model.
// Called at most a few times per second.
type Progress func(id string, done, total int64)

// Install downloads m's archive to a temp file in root, verifies its size, extracts it, and
// removes the archive. A failed attempt leaves no partial model directory behind: extraction
// goes to a temp dir that is renamed into place only on success.
func Install(root string, m Model, progress Progress) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	// Some models (speaker embeddings) are published as a bare .onnx file rather
	// than an archive: download into a temp dir and rename it into place whole.
	if strings.HasSuffix(m.URL, ".onnx") {
		tmpDir := filepath.Join(root, m.Dir+".extracting")
		if err := os.RemoveAll(tmpDir); err != nil {
			return err
		}
		if err := os.MkdirAll(tmpDir, 0o755); err != nil {
			return err
		}
		dest := filepath.Join(tmpDir, filepath.Base(m.URL))
		if err := download(m, dest, progress); err != nil {
			os.RemoveAll(tmpDir)
			return err
		}
		if err := os.Rename(tmpDir, filepath.Join(root, m.Dir)); err != nil {
			os.RemoveAll(tmpDir)
			return err
		}
		return nil
	}
	archive := filepath.Join(root, m.ID+".part")
	defer os.Remove(archive)
	if err := download(m, archive, progress); err != nil {
		return err
	}
	tmpDir := filepath.Join(root, m.Dir+".extracting")
	if err := os.RemoveAll(tmpDir); err != nil {
		return err
	}
	if err := extractTarBz2(archive, tmpDir); err != nil {
		os.RemoveAll(tmpDir)
		return fmt.Errorf("extract: %w", err)
	}
	// Archives normally contain a single top-level directory named m.Dir; a few wrap their files
	// in a differently-named directory, so fall back to whatever single directory is present.
	extracted := filepath.Join(tmpDir, m.Dir)
	if _, err := os.Stat(extracted); err != nil {
		entries, _ := os.ReadDir(tmpDir)
		var dirs []string
		for _, e := range entries {
			if e.IsDir() {
				dirs = append(dirs, e.Name())
			}
		}
		if len(dirs) != 1 {
			os.RemoveAll(tmpDir)
			return fmt.Errorf("archive did not contain a single model directory (found %v)", dirs)
		}
		extracted = filepath.Join(tmpDir, dirs[0])
	}
	if err := os.Rename(extracted, filepath.Join(root, m.Dir)); err != nil {
		os.RemoveAll(tmpDir)
		return err
	}
	return os.RemoveAll(tmpDir)
}

func download(m Model, dest string, progress Progress) error {
	client := &http.Client{Timeout: 0} // large downloads; per-read progress is the liveness signal
	resp, err := client.Get(m.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, m.URL)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	var done int64
	lastReport := time.Time{}
	buf := make([]byte, 256*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			done += int64(n)
			if progress != nil && time.Since(lastReport) > 300*time.Millisecond {
				progress(m.ID, done, m.Size)
				lastReport = time.Now()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if progress != nil {
		progress(m.ID, done, m.Size)
	}
	if m.Size > 0 && done != m.Size {
		return fmt.Errorf("size mismatch: got %d bytes, catalog says %d — truncated download or changed asset", done, m.Size)
	}
	return f.Sync()
}

// extractTarBz2 unpacks archive into destRoot, refusing entries that would escape it.
func extractTarBz2(archive, destRoot string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(bzip2.NewReader(f))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(hdr.Name)
		if name == "." || strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			return fmt.Errorf("archive entry escapes extraction root: %q", hdr.Name)
		}
		dest := filepath.Join(destRoot, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return err
			}
			out, err := os.Create(dest)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink, tar.TypeLink:
			log.Printf("models: skipping link entry %q in archive", hdr.Name) // not needed by any model
		default:
			return fmt.Errorf("unsupported archive entry type %d for %q", hdr.Typeflag, hdr.Name)
		}
	}
}
