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
	m := Model{Name: "fixture", Kind: TTS, URL: srv.URL, Size: int64(len(body)), Dir: "fixture-model"}

	var calls int
	if err := Install(root, m, func(name string, done, total int64) { calls++ }); err != nil {
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
	// No leftovers from the install process.
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Fatalf("stray files left in root: %v", entries)
	}
}

func TestInstallSizeMismatchFailsClean(t *testing.T) {
	body := fixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body[:len(body)-10]) // truncated transfer
	}))
	defer srv.Close()

	root := t.TempDir()
	m := Model{Name: "fixture", Kind: TTS, URL: srv.URL, Size: int64(len(body)), Dir: "fixture-model"}
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

func TestRegistryDefaultsResolve(t *testing.T) {
	d := Defaults()
	if len(d) != 2 || d[0].Kind != TTS || d[1].Kind != STT {
		t.Fatalf("unexpected defaults: %+v", d)
	}
}
