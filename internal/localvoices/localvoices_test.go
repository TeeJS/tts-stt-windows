package localvoices

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeJSON mimics the parts of a Piper <name>.onnx.json that matter here: the speaker name and a
// phoneme_id_map whose values are lists (Piper allows several ids per phoneme; sherpa takes the
// first). Includes the pad/bos/eos entries and the space phoneme.
const fakeJSON = `{
  "dataset": "my_voice",
  "audio": {"sample_rate": 22050},
  "phoneme_id_map": {
    "_": [0], "^": [1], "$": [2], " ": [3], "!": [4], "a": [10, 99], "ɑ": [11], "ʃ": [20]
  }
}`

// addVoice creates root\<name>\ with the two Piper files.
func addVoice(t *testing.T, root, name, meta string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".onnx"), []byte("not really onnx"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".onnx.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// espeakDir makes a stand-in espeak-ng-data folder with a nested file, so a copy is checked
// recursively.
func espeakDir(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "espeak-ng-data")
	if err := os.MkdirAll(filepath.Join(d, "lang"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"phondata", filepath.Join("lang", "en")} {
		if err := os.WriteFile(filepath.Join(d, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func TestIDRoundTrip(t *testing.T) {
	if got := ID("mine"); got != "local:mine" {
		t.Errorf("ID = %q", got)
	}
	if name, ok := Name("local:mine"); !ok || name != "mine" {
		t.Errorf("Name = %q, %v", name, ok)
	}
	for _, id := range []string{"", "mine", "pocket:mine", "piper-en-GB-alan-medium", "windows-builtin"} {
		if _, ok := Name(id); ok {
			t.Errorf("Name(%q) accepted a non-local id", id)
		}
	}
}

func TestListAndExists(t *testing.T) {
	root := t.TempDir()
	addVoice(t, root, "zeta", fakeJSON)
	addVoice(t, root, "alpha", `{"phoneme_id_map": {"_": [0]}}`) // no dataset: label from the name
	os.MkdirAll(filepath.Join(root, "incomplete"), 0o755)        // folder without files
	os.WriteFile(filepath.Join(root, "stray.onnx"), nil, 0o644)  // file at the root, not a voice

	got := List(root)
	if len(got) != 2 || got[0].Name != "alpha" || got[1].Name != "zeta" {
		t.Fatalf("List = %+v, want alpha then zeta", got)
	}
	if got[0].Label != "Alpha" || got[0].ID != "local:alpha" {
		t.Errorf("alpha: %+v", got[0])
	}
	if got[1].Label != "My Voice" {
		t.Errorf("zeta label = %q, want %q (title-cased dataset)", got[1].Label, "My Voice")
	}
	if got[1].Dir != filepath.Join(root, "zeta") {
		t.Errorf("zeta dir = %q", got[1].Dir)
	}
	if !Exists(root, "zeta") || Exists(root, "incomplete") || Exists(root, "nope") {
		t.Error("Exists is wrong about which folders hold a voice")
	}
	if List(filepath.Join(root, "missing")) != nil {
		t.Error("a missing root should list nothing, not fail")
	}
}

func TestNameSafety(t *testing.T) {
	root := t.TempDir()
	bad := []string{"", " ", ".", "..", "a/b", `a\b`, "..\\up", "../up", "nul\x00byte", "c:evil", "a<b", `q"uote`, "star*", "what?",
		"tab\tname", "bell\x07", "CON", "con", "Nul.old", "COM1", "lpt9"}
	for _, name := range bad {
		if _, err := Prepare(root, name, ""); !errors.Is(err, ErrBadName) {
			t.Errorf("Prepare(%q) = %v, want ErrBadName", name, err)
		}
		if Exists(root, name) {
			t.Errorf("Exists(%q) = true", name)
		}
	}
	for _, name := range []string{"Matthew Evenson", "T.J. Schmitz", "José", "Connie", "Lptop"} {
		if _, err := dirFor(root, name); err != nil {
			t.Errorf("dirFor(%q) = %v, want nil", name, err)
		}
	}
	// A folder outside root must never be touched even via a valid-looking name.
	if _, err := Prepare(root, "missing", ""); err == nil || errors.Is(err, ErrBadName) {
		t.Errorf("Prepare(missing voice) = %v, want a not-found error", err)
	}
}

func TestPrepareWritesTokensAndCopiesEspeak(t *testing.T) {
	root := t.TempDir()
	dir := addVoice(t, root, "mine", fakeJSON)
	espeak := espeakDir(t)

	got, err := Prepare(root, "mine", espeak)
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Errorf("Prepare returned %q, want %q", got, dir)
	}
	b, err := os.ReadFile(filepath.Join(dir, "tokens.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// sherpa's converter layout: "<phoneme> <first id>", sorted by phoneme (bytewise), so the
	// space phoneme comes first as a line that starts with a space.
	want := "  3\n! 4\n$ 2\n^ 1\n_ 0\na 10\nɑ 11\nʃ 20\n"
	if string(b) != want {
		t.Errorf("tokens.txt =\n%q\nwant\n%q", b, want)
	}
	// espeak-ng-data is present as a link or a copy — either way the nested file is reachable.
	if _, err := os.Stat(filepath.Join(dir, "espeak-ng-data", "lang", "en")); err != nil {
		t.Errorf("espeak-ng-data not usable: %v", err)
	}

	// Second call is a no-op: a hand-edited tokens.txt must survive.
	os.WriteFile(filepath.Join(dir, "tokens.txt"), []byte("custom\n"), 0o644)
	if _, err := Prepare(root, "mine", "does-not-exist"); err != nil {
		t.Fatalf("second Prepare: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "tokens.txt")); string(b) != "custom\n" {
		t.Error("Prepare overwrote an existing tokens.txt")
	}
}

func TestPrepareErrors(t *testing.T) {
	root := t.TempDir()
	addVoice(t, root, "nomap", `{"dataset": "x"}`)
	if _, err := Prepare(root, "nomap", espeakDir(t)); err == nil || !strings.Contains(err.Error(), "phoneme_id_map") {
		t.Errorf("json without phoneme_id_map: err = %v", err)
	}
	addVoice(t, root, "noespeak", fakeJSON)
	if _, err := Prepare(root, "noespeak", filepath.Join(root, "absent")); err == nil {
		t.Error("missing espeak source should fail, not silently produce an unloadable voice")
	}
}

func TestCopyDirIsUsedWhenLinkFails(t *testing.T) {
	// copyDir is the fallback path; exercise it directly so the test doesn't depend on whether
	// this machine may create directory symlinks.
	src := espeakDir(t)
	dst := filepath.Join(t.TempDir(), "copy")
	if err := copyDir(src, dst); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dst, "lang", "en"))
	if err != nil || string(b) != filepath.Join("lang", "en") {
		t.Errorf("copied file wrong: %q, %v", b, err)
	}
}

func TestTitleCase(t *testing.T) {
	for in, want := range map[string]string{"cori": "Cori", "my_voice-v2": "My Voice V2", "José  luis": "José Luis", "x": "X"} {
		if got := titleCase(in); got != want {
			t.Errorf("titleCase(%q) = %q, want %q", in, got, want)
		}
	}
}
