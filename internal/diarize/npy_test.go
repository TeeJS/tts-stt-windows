package diarize

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// numpyFixture is byte-for-byte what numpy.save writes for
// np.arange(4, dtype=np.float32): v1.0 header padded to 128 bytes total.
func numpyFixture() []byte {
	header := "{'descr': '<f4', 'fortran_order': False, 'shape': (4,), }" +
		string(bytes.Repeat([]byte{' '}, 60)) + "\n"
	b := []byte("\x93NUMPY\x01\x00")
	b = append(b, byte(len(header)), byte(len(header)>>8))
	b = append(b, header...)
	// float32 LE: 0, 1, 2, 3
	b = append(b,
		0, 0, 0, 0,
		0, 0, 0x80, 0x3f,
		0, 0, 0, 0x40,
		0, 0, 0x40, 0x40)
	return b
}

func TestReadNPYNumpyFixture(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fix.npy")
	if err := os.WriteFile(p, numpyFixture(), 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := ReadNPY(p)
	if err != nil {
		t.Fatal(err)
	}
	want := []float32{0, 1, 2, 3}
	if len(v) != len(want) {
		t.Fatalf("len = %d, want %d", len(v), len(want))
	}
	for i := range want {
		if v[i] != want[i] {
			t.Errorf("v[%d] = %v, want %v", i, v[i], want[i])
		}
	}
}

func TestWriteNPYMatchesNumpyBytes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.npy")
	if err := WriteNPY(p, []float32{0, 1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, numpyFixture()) {
		t.Errorf("WriteNPY output differs from numpy.save layout\ngot  %q\nwant %q", got, numpyFixture())
	}
}

func TestNPYRoundTrip256(t *testing.T) {
	v := make([]float32, 256)
	for i := range v {
		v[i] = float32(i) * 0.5
	}
	p := filepath.Join(t.TempDir(), "emb.npy")
	if err := WriteNPY(p, v); err != nil {
		t.Fatal(err)
	}
	got, err := ReadNPY(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 256 {
		t.Fatalf("len = %d", len(got))
	}
	for i := range v {
		if got[i] != v[i] {
			t.Fatalf("got[%d] = %v, want %v", i, got[i], v[i])
		}
	}
}

func TestReadNPYShape1xN(t *testing.T) {
	// Hand-build a (1, 4) header — some tooling saves row vectors this way.
	header := "{'descr': '<f4', 'fortran_order': False, 'shape': (1, 4), }"
	pad := (10 + len(header) + 1) % 64
	if pad != 0 {
		header += string(bytes.Repeat([]byte{' '}, 64-pad))
	}
	header += "\n"
	b := []byte("\x93NUMPY\x01\x00")
	b = append(b, byte(len(header)), byte(len(header)>>8))
	b = append(b, header...)
	b = append(b, make([]byte, 16)...)
	p := filepath.Join(t.TempDir(), "row.npy")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := ReadNPY(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 4 {
		t.Fatalf("len = %d, want 4", len(v))
	}
}

func TestReadNPYRejects(t *testing.T) {
	dir := t.TempDir()
	cases := map[string][]byte{
		"not-npy.npy": []byte("hello world, definitely not numpy"),
		"f8.npy": func() []byte {
			b := numpyFixture()
			return bytes.Replace(b, []byte("'<f4'"), []byte("'<f8'"), 1)
		}(),
		"truncated.npy": numpyFixture()[:20],
	}
	for name, data := range cases {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadNPY(p); err == nil {
			t.Errorf("%s: expected error, got none", name)
		}
	}
}
