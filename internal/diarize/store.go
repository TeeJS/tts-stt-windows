package diarize

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// EnrollmentStore holds one .npy voice profile per speaker; the filename (minus the
// extension) IS the speaker's identity, matching the Python service's layout so a
// profile folder can be copied between the two stacks or between machines.
type EnrollmentStore struct {
	dir string
}

var (
	ErrBadName  = errors.New("invalid speaker name")
	ErrNotFound = errors.New("speaker not found")
	ErrExists   = errors.New("speaker already exists")
)

func NewEnrollmentStore(dir string) (*EnrollmentStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	return &EnrollmentStore{dir: abs}, nil
}

// pathFor validates a speaker name arriving straight from an HTTP form field and maps
// it to its profile path. Port of enrollment.py's _path_for safety checks.
func (s *EnrollmentStore) pathFor(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("%w: %q", ErrBadName, name)
	}
	p := filepath.Join(s.dir, name+".npy")
	if filepath.Dir(p) != s.dir {
		return "", fmt.Errorf("%w: %q", ErrBadName, name)
	}
	return p, nil
}

func (s *EnrollmentStore) Save(name string, embedding []float32) error {
	p, err := s.pathFor(name)
	if err != nil {
		return err
	}
	return WriteNPY(p, embedding)
}

func (s *EnrollmentStore) Delete(name string) error {
	p, err := s.pathFor(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %q", ErrNotFound, name)
		}
		return err
	}
	return nil
}

// Rename preserves the profile file (and so its enrollment date, the mtime).
func (s *EnrollmentStore) Rename(from, to string) error {
	src, err := s.pathFor(from)
	if err != nil {
		return err
	}
	dst, err := s.pathFor(to)
	if err != nil {
		return err
	}
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return fmt.Errorf("%w: %q", ErrNotFound, from)
	}
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("%w: %q", ErrExists, to)
	}
	return os.Rename(src, dst)
}

// ListSpeakers returns enrolled names, sorted.
func (s *EnrollmentStore) ListSpeakers() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".npy") {
			names = append(names, strings.TrimSuffix(e.Name(), ".npy"))
		}
	}
	sort.Strings(names)
	return names, nil
}

type SpeakerDetail struct {
	Name       string `json:"name"`
	EnrolledAt string `json:"enrolled_at"` // RFC3339 UTC, from the profile file's mtime
}

func (s *EnrollmentStore) ListDetails() ([]SpeakerDetail, error) {
	names, err := s.ListSpeakers()
	if err != nil {
		return nil, err
	}
	details := make([]SpeakerDetail, 0, len(names))
	for _, n := range names {
		d := SpeakerDetail{Name: n}
		if fi, err := os.Stat(filepath.Join(s.dir, n+".npy")); err == nil {
			d.EnrolledAt = fi.ModTime().UTC().Format(time.RFC3339)
		}
		details = append(details, d)
	}
	return details, nil
}

// AllEmbeddings loads every profile. Unreadable files are skipped rather than failing
// the whole identification run.
func (s *EnrollmentStore) AllEmbeddings() (map[string][]float32, error) {
	names, err := s.ListSpeakers()
	if err != nil {
		return nil, err
	}
	out := make(map[string][]float32, len(names))
	for _, n := range names {
		emb, err := ReadNPY(filepath.Join(s.dir, n+".npy"))
		if err != nil {
			continue
		}
		out[n] = emb
	}
	return out, nil
}
