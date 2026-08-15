package diarize

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Voice profiles are stored as NumPy .npy files (one float32 vector per speaker) for
// bidirectional compatibility with the Python meeting-diarizer: profiles written by either
// stack load in the other, and users can back up or move machines by copying the folder.

var npyMagic = []byte("\x93NUMPY")

var npyShapeRe = regexp.MustCompile(`'shape':\s*\(([0-9, ]*)\)`)

// ReadNPY reads a 1-D float32 little-endian .npy file (shape (N,) or (1, N)).
func ReadNPY(path string) ([]float32, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) < 10 || string(b[:6]) != string(npyMagic) {
		return nil, fmt.Errorf("%s: not a .npy file", path)
	}
	major := b[6]
	var headerLen, headerStart int
	switch major {
	case 1:
		headerLen = int(binary.LittleEndian.Uint16(b[8:10]))
		headerStart = 10
	case 2, 3:
		if len(b) < 12 {
			return nil, fmt.Errorf("%s: truncated .npy header", path)
		}
		headerLen = int(binary.LittleEndian.Uint32(b[8:12]))
		headerStart = 12
	default:
		return nil, fmt.Errorf("%s: unsupported .npy version %d", path, major)
	}
	if len(b) < headerStart+headerLen {
		return nil, fmt.Errorf("%s: truncated .npy header", path)
	}
	header := string(b[headerStart : headerStart+headerLen])
	if !strings.Contains(header, "'descr': '<f4'") {
		return nil, fmt.Errorf("%s: expected float32 ('<f4') data, header: %s", path, strings.TrimSpace(header))
	}
	if strings.Contains(header, "'fortran_order': True") {
		return nil, fmt.Errorf("%s: fortran-order arrays not supported", path)
	}
	m := npyShapeRe.FindStringSubmatch(header)
	if m == nil {
		return nil, fmt.Errorf("%s: no shape in .npy header", path)
	}
	n := 1
	dims := 0
	for _, f := range strings.Split(m[1], ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		d, err := strconv.Atoi(f)
		if err != nil {
			return nil, fmt.Errorf("%s: bad shape %q", path, m[1])
		}
		n *= d
		dims++
		if dims > 2 || (dims == 2 && d != n) { // 2-D allowed only as (1, N)
			return nil, fmt.Errorf("%s: expected a 1-D vector, shape (%s)", path, m[1])
		}
	}
	data := b[headerStart+headerLen:]
	if len(data) < 4*n {
		return nil, fmt.Errorf("%s: expected %d float32 values, have %d bytes", path, n, len(data))
	}
	out := make([]float32, n)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[4*i:]))
	}
	return out, nil
}

// WriteNPY writes v as a v1.0 .npy file with shape (len(v),), byte-identical to
// what numpy.save produces for a float32 vector.
func WriteNPY(path string, v []float32) error {
	header := fmt.Sprintf("{'descr': '<f4', 'fortran_order': False, 'shape': (%d,), }", len(v))
	// Total of magic(6) + version(2) + lenfield(2) + header must be a multiple of 64,
	// with the header space-padded and newline-terminated (numpy's own layout).
	total := 10 + len(header) + 1
	if pad := total % 64; pad != 0 {
		header += strings.Repeat(" ", 64-pad)
	}
	header += "\n"

	buf := make([]byte, 0, 10+len(header)+4*len(v))
	buf = append(buf, npyMagic...)
	buf = append(buf, 1, 0)
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(header)))
	buf = append(buf, header...)
	for _, f := range v {
		buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(f))
	}
	return os.WriteFile(path, buf, 0o644)
}
