package models

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/fixture-model.tar.bz2")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestInstallAndVerify(t *testing.T) {
	body := fixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	root := t.TempDir()
	m := Model{ID: "fixture", Kind: TTS, URL: srv.URL, Size: int64(len(body)), Dir: "fixture-model"}

	var calls int
	if err := Install(root, m, func(id string, done, total int64) { calls++ }); err != nil {
		t.Fatal(err)
	}
	if calls == 0 {
		t.Error("no progress callbacks")
	}
	if !Installed(root, m) {
		t.Fatal("model not installed")
	}
	data, err := os.ReadFile(filepath.Join(root, "fixture-model", "data.txt"))
	if err != nil || string(data) != "hello model\n" {
		t.Fatalf("extracted content wrong: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(root, "fixture-model", "sub", "n.txt")); err != nil {
		t.Fatalf("nested file missing: %v", err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Fatalf("stray files left in root: %v", entries)
	}
	if err := Remove(root, m); err != nil {
		t.Fatal(err)
	}
	if Installed(root, m) {
		t.Fatal("model still present after Remove")
	}
}

func TestInstallSizeMismatchFailsClean(t *testing.T) {
	body := fixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body[:len(body)-10]) // truncated transfer
	}))
	defer srv.Close()

	root := t.TempDir()
	m := Model{ID: "fixture", Kind: TTS, URL: srv.URL, Size: int64(len(body)), Dir: "fixture-model"}
	if err := Install(root, m, nil); err == nil {
		t.Fatal("truncated download should fail")
	}
	if Installed(root, m) {
		t.Fatal("failed install must not leave a model directory")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatalf("stray files left after failure: %v", entries)
	}
}

// The embedded catalog is generated at build time; these assertions catch a generator regression
// (empty list, missing languages, duplicate ids) before it ships.
func TestCatalogSane(t *testing.T) {
	all := All()
	if len(all) < 200 {
		t.Fatalf("catalog looks truncated: %d entries", len(all))
	}
	seen := map[string]bool{}
	var voices, speech int
	for _, m := range all {
		if seen[m.ID] {
			t.Errorf("duplicate id %q", m.ID)
		}
		seen[m.ID] = true
		if m.Name == "" || m.URL == "" || m.Dir == "" || m.Size <= 0 || len(m.Langs) == 0 {
			t.Errorf("incomplete entry: %+v", m)
		}
		switch m.Kind {
		case TTS:
			voices++
		case STT:
			speech++
		case Diar:
		default:
			t.Errorf("bad kind %q on %s", m.Kind, m.ID)
		}
		for _, l := range m.Langs {
			if _, ok := Languages()[l]; !ok {
				t.Errorf("%s uses language %q with no display name", m.ID, l)
			}
		}
	}
	if voices < 150 || speech < 20 {
		t.Fatalf("unexpected split: %d voices, %d speech models", voices, speech)
	}
}

// Every language with a voice must resolve to a usable default pair, or a user picking that
// language on first run gets nothing.
func TestDefaultsForEveryLanguage(t *testing.T) {
	langs := map[string]bool{}
	for _, m := range OfKind(TTS) {
		for _, l := range m.Langs {
			if l != "multi" {
				langs[l] = true
			}
		}
	}
	if len(langs) < 40 {
		t.Fatalf("only %d languages have voices", len(langs))
	}
	for lang := range langs {
		voice, stt := DefaultsFor(lang)
		if voice.ID == "" {
			t.Errorf("no default voice for %q", lang)
		}
		if stt.ID == "" {
			t.Errorf("no default speech model for %q", lang)
		}
	}
}

func TestForLanguageIncludesMultilingual(t *testing.T) {
	// A Georgian speaker has no Georgian-only speech model, so the multi-language models must
	// still be offered — otherwise the list would be empty.
	got := ForLanguage(STT, "ka")
	if len(got) == 0 {
		t.Fatal("no speech models offered for Georgian")
	}
	var multi bool
	for _, m := range got {
		for _, l := range m.Langs {
			if l == "multi" {
				multi = true
			}
		}
	}
	if !multi {
		t.Error("expected multi-language models in the list")
	}
}
