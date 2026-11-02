package voices

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// clip returns secs seconds of a quiet 220 Hz tone at SampleRate.
func clip(secs float64) []float32 {
	n := int(secs * SampleRate)
	out := make([]float32, n)
	for i := range out {
		out[i] = 0.25 * float32(math.Sin(2*math.Pi*220*float64(i)/SampleRate))
	}
	return out
}

func TestIDRoundTrip(t *testing.T) {
	if got := ID("bria"); got != "pocket:bria" {
		t.Errorf("ID = %q", got)
	}
	if name, ok := Name("pocket:bria"); !ok || name != "bria" {
		t.Errorf("Name = %q, %v", name, ok)
	}
	for _, id := range []string{"", "bria", "piper-en-GB-alan-medium", "windows-builtin", "pocket-tts"} {
		if _, ok := Name(id); ok {
			t.Errorf("Name(%q) accepted a non-clone id", id)
		}
	}
}

func TestStorePathSafety(t *testing.T) {
	s := newTestStore(t)
	bad := []string{"", " ", ".", "..", "a/b", `a\b`, "..\\up", "../up", "nul\x00byte", "c:evil", "a<b", `q"uote`, "star*", "what?",
		"tab\tname", "bell\x07", "CON", "con", "Nul.old", "COM1", "lpt9"}
	for _, name := range bad {
		if err := s.Save(name, clip(5)); !errors.Is(err, ErrBadName) {
			t.Errorf("Save(%q) = %v, want ErrBadName", name, err)
		}
	}
	for _, name := range []string{"Matthew Evenson", "T.J. Schmitz", "José", "Connie", "Lptop"} {
		if err := s.Save(name, clip(5)); err != nil {
			t.Errorf("Save(%q) = %v, want nil", name, err)
		}
	}
}

func TestStoreLengthBounds(t *testing.T) {
	s := newTestStore(t)
	if err := s.Save("short", clip(MinSeconds-0.5)); !errors.Is(err, ErrTooShort) {
		t.Errorf("short clip: %v, want ErrTooShort", err)
	}
	if err := s.Save("long", clip(MaxSeconds+1)); !errors.Is(err, ErrTooLong) {
		t.Errorf("long clip: %v, want ErrTooLong", err)
	}
	if err := s.Save("ok", clip(MinSeconds)); err != nil {
		t.Errorf("minimum-length clip: %v", err)
	}
}

// Windows file names are case-insensitive, so a differently-cased name is the same clip and
// must never silently overwrite it; an exact repeat is a mistake too.
func TestStoreNoOverwrite(t *testing.T) {
	s := newTestStore(t)
	if err := s.Save("Bob", clip(5)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Bob", "bob", "BOB", " bob "} {
		if err := s.Save(name, clip(6)); !errors.Is(err, ErrExists) {
			t.Errorf("Save(%q) over existing = %v, want ErrExists", name, err)
		}
	}
	if err := s.Save("Alice", clip(5)); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename("Alice", "bob"); !errors.Is(err, ErrExists) {
		t.Errorf("Rename onto existing (other case) = %v, want ErrExists", err)
	}
	if err := s.Rename("Bob", "BOB"); err != nil { // case-only rename of the same file is fine
		t.Errorf("case-only rename = %v", err)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	s := newTestStore(t)
	in := clip(4.5)
	if err := s.Save("v", in); err != nil {
		t.Fatal(err)
	}
	out, rate, err := s.Load("v")
	if err != nil {
		t.Fatal(err)
	}
	if rate != SampleRate || len(out) != len(in) {
		t.Fatalf("Load: rate %d len %d, want %d / %d", rate, len(out), SampleRate, len(in))
	}
	for i := 0; i < len(in); i += 997 {
		if d := math.Abs(float64(in[i] - out[i])); d > 1.0/32767*2 {
			t.Fatalf("sample %d: %v vs %v", i, in[i], out[i])
		}
	}
	d, err := s.ListDetails()
	if err != nil || len(d) != 1 || d[0].Name != "v" || d[0].ID != "pocket:v" || math.Abs(d[0].Seconds-4.5) > 0.01 {
		t.Errorf("ListDetails = %+v, %v", d, err)
	}
	if !s.Exists("v") || s.Exists("w") || s.Exists("../v") {
		t.Error("Exists")
	}
	if err := s.Delete("v"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("v"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Delete = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "v.wav")); !os.IsNotExist(err) {
		t.Error("file still present after Delete")
	}
}

// Only .wav files are voices: a stray file in the folder is ignored, not listed.
func TestStoreListIgnoresOtherFiles(t *testing.T) {
	s := newTestStore(t)
	os.WriteFile(filepath.Join(s.dir, "notes.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(s.dir, "v.wav.tmp"), []byte("x"), 0o644)
	if err := s.Save("real", clip(5)); err != nil {
		t.Fatal(err)
	}
	names, _ := s.List()
	if len(names) != 1 || names[0] != "real" {
		t.Errorf("List = %v", names)
	}
}
