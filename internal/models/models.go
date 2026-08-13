// Package models manages the on-disk model store (%APPDATA%\tts-sst\models): a pinned registry
// of known-good sherpa-onnx release assets, plus download-and-extract with progress. Archives are
// tar.bz2, extracted with the pure-Go stdlib readers — no external tools.
package models

import (
	"archive/tar"
	"compress/bzip2"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Kind string

const (
	STT Kind = "stt"
	TTS Kind = "tts"
)

// Model is one downloadable entry. Size is the exact archive byte count (verified after
// download); Dir is the directory name the archive extracts to under the models root.
type Model struct {
	Name        string
	Kind        Kind
	URL         string
	Size        int64
	Dir         string
	Description string
}

// Registry is the pinned catalog. URLs and sizes captured 2026-08-13 from the sherpa-onnx
// release assets; these are stable, versioned release files, not moving targets.
var Registry = []Model{
	{
		Name: "piper-lessac-medium", Kind: TTS,
		URL:  "https://github.com/k2-fsa/sherpa-onnx/releases/download/tts-models/vits-piper-en_US-lessac-medium.tar.bz2",
		Size: 67230653, Dir: "vits-piper-en_US-lessac-medium",
		Description: "Piper voice, US English (default)",
	},
	{
		Name: "whisper-small-en", Kind: STT,
		URL:  "https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/sherpa-onnx-whisper-small.en.tar.bz2",
		Size: 635693775, Dir: "sherpa-onnx-whisper-small.en",
		Description: "Whisper small English (default; best quality)",
	},
	{
		Name: "whisper-base-en", Kind: STT,
		URL:  "https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/sherpa-onnx-whisper-base.en.tar.bz2",
		Size: 208576005, Dir: "sherpa-onnx-whisper-base.en",
		Description: "Whisper base English (smaller/faster, lower accuracy)",
	},
}

// Defaults returns the registry entries installed on first run: one STT + one TTS.
func Defaults() []Model {
	return []Model{byName("piper-lessac-medium"), byName("whisper-small-en")}
}

func byName(name string) Model {
	for _, m := range Registry {
		if m.Name == name {
			return m
		}
	}
	panic("models: unknown registry name " + name) // registry is compile-time data; a miss is a code bug
}

// Installed reports whether the model's directory exists in the store.
func Installed(root string, m Model) bool {
	fi, err := os.Stat(filepath.Join(root, m.Dir))
	return err == nil && fi.IsDir()
}

// Progress receives download state: bytes done of total for the named model.
// Called at most a few times per second.
type Progress func(name string, done, total int64)

// EnsureDefaults downloads any missing default models into root. Returns the first error;
// already-installed models are never touched.
func EnsureDefaults(root string, progress Progress) error {
	for _, m := range Defaults() {
		if Installed(root, m) {
			continue
		}
		if err := Install(root, m, progress); err != nil {
			return fmt.Errorf("install %s: %w", m.Name, err)
		}
	}
	return nil
}

// Install downloads m's archive to a temp file in root, verifies its size, extracts it, and
// removes the archive. A failed attempt leaves no partial model directory behind: extraction
// goes to a temp dir that is renamed into place only on success.
func Install(root string, m Model, progress Progress) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	archive := filepath.Join(root, m.Name+".part")
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
	// Archives contain a single top-level directory named m.Dir; move it into place.
	extracted := filepath.Join(tmpDir, m.Dir)
	if _, err := os.Stat(extracted); err != nil {
		os.RemoveAll(tmpDir)
		return fmt.Errorf("archive did not contain expected directory %s", m.Dir)
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
			if progress != nil && time.Since(lastReport) > 500*time.Millisecond {
				progress(m.Name, done, m.Size)
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
		progress(m.Name, done, m.Size)
	}
	if done != m.Size {
		return fmt.Errorf("size mismatch: got %d bytes, pinned %d — truncated download or changed asset", done, m.Size)
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
		default:
			// Symlinks etc. don't appear in these archives; refuse rather than mis-handle.
			return fmt.Errorf("unsupported archive entry type %d for %q", hdr.Typeflag, hdr.Name)
		}
	}
}
