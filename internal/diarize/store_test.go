package diarize

import (
	"errors"
	"testing"
)

func newTestStore(t *testing.T) *EnrollmentStore {
	t.Helper()
	s, err := NewEnrollmentStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStorePathSafety(t *testing.T) {
	s := newTestStore(t)
	bad := []string{"", " ", ".", "..", "a/b", `a\b`, "..\\up", "../up", "nul\x00byte"}
	for _, name := range bad {
		if err := s.Save(name, []float32{1}); !errors.Is(err, ErrBadName) {
			t.Errorf("Save(%q) = %v, want ErrBadName", name, err)
		}
	}
	// Names with spaces, dots and unicode are legal — they're real people's names.
	for _, name := range []string{"Matthew Evenson", "T.J. Schmitz", "José"} {
		if err := s.Save(name, []float32{1, 2}); err != nil {
			t.Errorf("Save(%q) = %v, want nil", name, err)
		}
	}
}

func TestStoreLifecycle(t *testing.T) {
	s := newTestStore(t)
	emb := []float32{0.1, 0.2, 0.3}
	if err := s.Save("Alice", emb); err != nil {
		t.Fatal(err)
	}
	if err := s.Save("Bob", []float32{1, 0, 0}); err != nil {
		t.Fatal(err)
	}

	names, err := s.ListSpeakers()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "Alice" || names[1] != "Bob" {
		t.Fatalf("ListSpeakers = %v", names)
	}

	details, err := s.ListDetails()
	if err != nil {
		t.Fatal(err)
	}
	if len(details) != 2 || details[0].EnrolledAt == "" {
		t.Fatalf("ListDetails = %+v", details)
	}

	all, err := s.AllEmbeddings()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || len(all["Alice"]) != 3 || all["Alice"][1] != emb[1] {
		t.Fatalf("AllEmbeddings = %v", all)
	}

	if err := s.Rename("Alice", "Bob"); !errors.Is(err, ErrExists) {
		t.Errorf("Rename onto existing = %v, want ErrExists", err)
	}
	if err := s.Rename("Nobody", "X"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Rename missing = %v, want ErrNotFound", err)
	}
	if err := s.Rename("Alice", "Alice Cooper"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("Bob"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("Bob"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete missing = %v, want ErrNotFound", err)
	}
	names, _ = s.ListSpeakers()
	if len(names) != 1 || names[0] != "Alice Cooper" {
		t.Fatalf("final ListSpeakers = %v", names)
	}
}
