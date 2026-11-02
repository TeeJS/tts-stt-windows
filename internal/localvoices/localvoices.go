// Package localvoices finds user-supplied Piper voices that are not in the embedded catalog —
// typically ones the user trained themselves. A voice is a folder %APPDATA%\tts-sst\piper\<name>\
// holding the standard Piper export, <name>.onnx + <name>.onnx.json, and is selected with the id
// "local:<name>" (see IDPrefix), the same one-string convention as catalog and cloned voices.
//
// sherpa-onnx's vits-piper loader wants two more things a Piper export doesn't ship with: a
// tokens.txt (derived here from the json's phoneme_id_map, in the layout sherpa's own converter
// writes) and an espeak-ng-data directory (linked or copied from an installed catalog voice).
// Prepare adds both the first time a voice is loaded, so a user only ever drops in two files.
package localvoices

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// IDPrefix marks a local voice in cfg.TTSVoice: "local:<name>".
const IDPrefix = "local:"

var ErrBadName = errors.New("invalid voice name")

// Voice is one local voice found under the root folder.
type Voice struct {
	Name  string `json:"name"`  // folder and file stem
	ID    string `json:"id"`    // "local:<name>", what the voice picker sends
	Label string `json:"label"` // display name: the json's "dataset" (or the name), title-cased
	Dir   string `json:"-"`     // the voice folder, what engine.NewPiperTTS loads
}

// ID returns the config id for a voice name; Name is its inverse (ok=false for other ids).
func ID(name string) string { return IDPrefix + name }
func Name(id string) (string, bool) {
	if !strings.HasPrefix(id, IDPrefix) {
		return "", false
	}
	return strings.TrimPrefix(id, IDPrefix), true
}

// dirFor validates a name (same rules as the cloned-voice store: no path characters, no DOS
// device names) and maps it to its folder under root.
func dirFor(root, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, `/\:<>"|?*`) || strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) ||
		reservedName(name) {
		return "", fmt.Errorf("%w: %q", ErrBadName, name)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	p := filepath.Join(abs, name)
	if filepath.Dir(p) != abs {
		return "", fmt.Errorf("%w: %q", ErrBadName, name)
	}
	return p, nil
}

// reservedName reports a DOS device name (CON, PRN, AUX, NUL, COM1–9, LPT1–9), which Windows
// refuses as a file name whatever the extension; compared case-insensitively like the FS does.
func reservedName(name string) bool {
	up := strings.ToUpper(strings.TrimSpace(name))
	if i := strings.IndexByte(up, '.'); i >= 0 {
		up = up[:i]
	}
	switch up {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	return len(up) == 4 && (up[:3] == "COM" || up[:3] == "LPT") && up[3] >= '1' && up[3] <= '9'
}

func modelPath(dir, name string) string { return filepath.Join(dir, name+".onnx") }
func jsonPath(dir, name string) string  { return filepath.Join(dir, name+".onnx.json") }

// Exists reports whether root\<name>\ holds both Piper files (false for invalid names too).
func Exists(root, name string) bool {
	_, ok := Get(root, name)
	return ok
}

// Get describes one voice, or ok=false when its folder or files are missing.
func Get(root, name string) (Voice, bool) {
	dir, err := dirFor(root, name)
	if err != nil {
		return Voice{}, false
	}
	name = strings.TrimSpace(name)
	if !isFile(modelPath(dir, name)) || !isFile(jsonPath(dir, name)) {
		return Voice{}, false
	}
	return Voice{Name: name, ID: ID(name), Dir: dir, Label: label(jsonPath(dir, name), name)}, true
}

// List returns every voice under root, sorted by name. A missing root is simply empty.
func List(root string) []Voice {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []Voice
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if v, ok := Get(root, e.Name()); ok {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// label reads the json's "dataset" (Piper's name for the speaker, e.g. "cori") and title-cases it;
// an unreadable json falls back to the folder name.
func label(jsonFile, name string) string {
	var meta struct {
		Dataset string `json:"dataset"`
	}
	if b, err := os.ReadFile(jsonFile); err == nil {
		_ = json.Unmarshal(b, &meta)
	}
	s := strings.TrimSpace(meta.Dataset)
	if s == "" {
		s = name
	}
	return titleCase(s)
}

// titleCase upper-cases the first letter of each word; "_" and "-" count as word breaks and
// become spaces, so "my_voice-v2" reads "My Voice V2".
func titleCase(s string) string {
	words := strings.FieldsFunc(s, func(r rune) bool { return r == '_' || r == '-' || unicode.IsSpace(r) })
	for i, w := range words {
		r := []rune(w)
		r[0] = unicode.ToUpper(r[0])
		words[i] = string(r)
	}
	return strings.Join(words, " ")
}

// Prepare makes root\<name>\ loadable by sherpa-onnx's vits-piper loader and returns the folder:
// tokens.txt is written from the json when missing, and espeak-ng-data is linked to (or, if the
// link cannot be made, copied from) espeakDataDir when missing. Existing files are left alone, so
// a user who ships their own tokens.txt keeps it.
func Prepare(root, name, espeakDataDir string) (string, error) {
	v, ok := Get(root, name)
	if !ok {
		if _, err := dirFor(root, name); err != nil {
			return "", err
		}
		return "", fmt.Errorf("local voice %q: %s.onnx and %s.onnx.json not found in %s", name, name, name, filepath.Join(root, name))
	}
	tokens := filepath.Join(v.Dir, "tokens.txt")
	if !isFile(tokens) {
		if err := writeTokens(jsonPath(v.Dir, v.Name), tokens); err != nil {
			return "", fmt.Errorf("local voice %q: tokens.txt: %w", name, err)
		}
	}
	data := filepath.Join(v.Dir, "espeak-ng-data")
	if !isDir(data) {
		if !isDir(espeakDataDir) {
			return "", fmt.Errorf("local voice %q: no espeak-ng-data to borrow (%s is not a directory)", name, espeakDataDir)
		}
		if err := os.Symlink(espeakDataDir, data); err != nil {
			// Directory symlinks need a privilege (or Developer Mode) on Windows; a copy always works.
			os.RemoveAll(data)
			if cerr := copyDir(espeakDataDir, data); cerr != nil {
				os.RemoveAll(data)
				return "", fmt.Errorf("local voice %q: espeak-ng-data: link failed (%v) and copy failed: %w", name, err, cerr)
			}
		}
	}
	return v.Dir, nil
}

// writeTokens emits the tokens.txt sherpa-onnx's piper converter (scripts/piper) produces from
// a Piper json: one "<phoneme> <id>" line per phoneme_id_map entry, first id only, sorted by
// phoneme. The pad "_", bos "^", eos "$" and the space phoneme (a line starting with a space) are
// ordinary entries; sherpa's token reader handles the leading-space line itself.
func writeTokens(jsonFile, dst string) error {
	b, err := os.ReadFile(jsonFile)
	if err != nil {
		return err
	}
	var meta struct {
		PhonemeIDMap map[string][]int `json:"phoneme_id_map"`
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return fmt.Errorf("parse %s: %w", filepath.Base(jsonFile), err)
	}
	if len(meta.PhonemeIDMap) == 0 {
		return fmt.Errorf("%s has no phoneme_id_map", filepath.Base(jsonFile))
	}
	lines := make([]string, 0, len(meta.PhonemeIDMap))
	for ph, ids := range meta.PhonemeIDMap {
		if len(ids) == 0 {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s %d", ph, ids[0]))
	}
	sort.Strings(lines)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

func isFile(p string) bool { fi, err := os.Stat(p); return err == nil && !fi.IsDir() }
func isDir(p string) bool  { fi, err := os.Stat(p); return err == nil && fi.IsDir() }
