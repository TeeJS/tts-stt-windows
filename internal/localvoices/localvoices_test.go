package localvoices

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeJSON mimics the parts of a Piper <name>.onnx.json that matter here: the speaker name and a
// phoneme_id_map whose values are lists (Piper allows several ids per phoneme; sherpa takes the
// first). Includes the pad/bos/eos entries and the space phoneme.
// "aɪ" is a diphthong the piper1-gpl trainer adds: two codepoints, which sherpa's token reader
// rejects, so it must be left out of tokens.txt.
const fakeJSON = `{
  "dataset": "my_voice",
  "audio": {"sample_rate": 22050},
  "espeak": {"voice": "en-gb-x-rp"},
  "language": {"name_english": "English"},
  "num_speakers": 2,
  "phoneme_id_map": {
    "_": [0], "^": [1], "$": [2], " ": [3], "!": [4], "a": [10, 99], "ɑ": [11], "ʃ": [20], "aɪ": [161]
  }
}`

// fakeONNX is a minimal but valid ModelProto: ir_version (field 1, varint) = 7 and
// producer_name (field 2, string) = "test". Prepare appends metadata_props after it.
var fakeONNX = []byte{0x08, 0x07, 0x12, 0x04, 't', 'e', 's', 't'}

// metadataIn decodes a serialized ModelProto's top-level fields and returns its metadata_props
// (field 14) as key/value pairs, plus the other field numbers seen — enough to prove the
// appended bytes are well-formed protobuf that leaves the original fields intact.
func metadataIn(t *testing.T, b []byte) (props [][2]string, fields []int) {
	t.Helper()
	readField := func(b []byte) (field, wire int, payload []byte, rest []byte) {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			t.Fatalf("bad tag at %d bytes from end", len(b))
		}
		b = b[n:]
		field, wire = int(tag>>3), int(tag&7)
		switch wire {
		case 0:
			_, n := binary.Uvarint(b)
			return field, wire, b[:n], b[n:]
		case 2:
			l, n := binary.Uvarint(b)
			b = b[n:]
			return field, wire, b[:l], b[l:]
		}
		t.Fatalf("unexpected wire type %d", wire)
		return
	}
	for len(b) > 0 {
		var field, wire int
		var payload []byte
		field, wire, payload, b = readField(b)
		fields = append(fields, field)
		if field != 14 || wire != 2 {
			continue
		}
		var kv [2]string
		for len(payload) > 0 {
			var f int
			var p []byte
			f, _, p, payload = readField(payload)
			kv[f-1] = string(p)
		}
		props = append(props, kv)
	}
	return props, fields
}

// addVoice creates root\<name>\ with the two Piper files.
func addVoice(t *testing.T, root, name, meta string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".onnx"), fakeONNX, 0o644); err != nil {
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

func TestPrepareBuildsSherpaFolder(t *testing.T) {
	root := t.TempDir()
	dir := addVoice(t, root, "mine", fakeJSON)
	espeak := espeakDir(t)

	got, err := Prepare(root, "mine", espeak)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, SherpaDir)
	if got != out {
		t.Errorf("Prepare returned %q, want %q", got, out)
	}
	b, err := os.ReadFile(filepath.Join(out, "tokens.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// sherpa's converter layout: "<phoneme> <first id>", sorted by phoneme (bytewise), so the
	// space phoneme comes first as a line that starts with a space. No "aɪ" line.
	want := "  3\n! 4\n$ 2\n^ 1\n_ 0\na 10\nɑ 11\nʃ 20\n"
	if string(b) != want {
		t.Errorf("tokens.txt =\n%q\nwant\n%q", b, want)
	}
	// espeak-ng-data is present as a link or a copy — either way the nested file is reachable.
	if _, err := os.Stat(filepath.Join(out, "espeak-ng-data", "lang", "en")); err != nil {
		t.Errorf("espeak-ng-data not usable: %v", err)
	}
	// The user's model is untouched; the copy is the original plus sherpa's metadata.
	if orig, _ := os.ReadFile(filepath.Join(dir, "mine.onnx")); string(orig) != string(fakeONNX) {
		t.Error("Prepare modified the user's .onnx")
	}
	model, err := os.ReadFile(filepath.Join(out, "mine.onnx"))
	if err != nil {
		t.Fatal(err)
	}
	if string(model[:len(fakeONNX)]) != string(fakeONNX) {
		t.Error("copied model does not start with the original bytes")
	}
	props, fields := metadataIn(t, model)
	wantProps := [][2]string{
		{"model_type", "vits"}, {"comment", "piper"}, {"language", "English"}, {"voice", "en-gb-x-rp"},
		{"has_espeak", "1"}, {"n_speakers", "2"}, {"sample_rate", "22050"},
	}
	if !reflect.DeepEqual(props, wantProps) {
		t.Errorf("metadata_props = %v, want %v", props, wantProps)
	}
	if !reflect.DeepEqual(fields[:2], []int{1, 2}) {
		t.Errorf("original fields not preserved: %v", fields)
	}

	// Unchanged sources: a second call reuses the folder (same mtime on the copy).
	fi1, _ := os.Stat(filepath.Join(out, "mine.onnx"))
	if _, err := Prepare(root, "mine", "does-not-exist"); err != nil {
		t.Fatalf("second Prepare: %v", err)
	}
	fi2, _ := os.Stat(filepath.Join(out, "mine.onnx"))
	if !fi1.ModTime().Equal(fi2.ModTime()) {
		t.Error("Prepare rebuilt the model copy although nothing changed")
	}

	// A newer source .onnx (the user re-exported) must be picked up.
	future := time.Now().Add(time.Hour)
	os.WriteFile(filepath.Join(dir, "mine.onnx"), append(fakeONNX, 0x12, 0x01, 'v'), 0o644)
	os.Chtimes(filepath.Join(dir, "mine.onnx"), future, future)
	if _, err := Prepare(root, "mine", "does-not-exist"); err != nil {
		t.Fatalf("third Prepare: %v", err)
	}
	model, _ = os.ReadFile(filepath.Join(out, "mine.onnx"))
	if len(model) != len(fakeONNX)+3+len(encodeMetadata(wantProps)) {
		t.Errorf("model copy not rebuilt from the newer source: %d bytes", len(model))
	}
}

func TestEncodeMetadataDefaults(t *testing.T) {
	// A raw piper1-gpl export has no language block and may omit espeak.voice / num_speakers.
	var m piperMeta
	m.Audio.SampleRate = 16000
	b := encodeMetadata(metadataProps(m))
	props, _ := metadataIn(t, b)
	want := [][2]string{
		{"model_type", "vits"}, {"comment", "piper"}, {"language", "English"}, {"voice", "en-us"},
		{"has_espeak", "1"}, {"n_speakers", "1"}, {"sample_rate", "16000"},
	}
	if !reflect.DeepEqual(props, want) {
		t.Errorf("props = %v, want %v", props, want)
	}
	// Byte-level check of one entry: tag 0x72, length, then key/value sub-fields.
	one := encodeMetadata([][2]string{{"k", "vv"}})
	if string(one) != string([]byte{0x72, 7, 0x0A, 1, 'k', 0x12, 2, 'v', 'v'}) {
		t.Errorf("encoding = % x", one)
	}
}

func TestPrepareErrors(t *testing.T) {
	root := t.TempDir()
	addVoice(t, root, "nomap", `{"dataset": "x", "audio": {"sample_rate": 22050}}`)
	if _, err := Prepare(root, "nomap", espeakDir(t)); err == nil || !strings.Contains(err.Error(), "phoneme_id_map") {
		t.Errorf("json without phoneme_id_map: err = %v", err)
	}
	addVoice(t, root, "norate", `{"phoneme_id_map": {"_": [0]}}`)
	if _, err := Prepare(root, "norate", espeakDir(t)); err == nil || !strings.Contains(err.Error(), "sample_rate") {
		t.Errorf("json without audio.sample_rate: err = %v", err)
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
