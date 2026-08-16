package diarize

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"

	mp3 "github.com/hajimehoshi/go-mp3"
)

// The pipeline consumes mono float32 at 16 kHz. Uploads arrive as WAV from arbitrary
// recorders — and sometimes as MP3 data wearing a .wav name (the HiDock P1 does this) —
// so decoding sniffs content, never trusts the filename.

const pipelineRate = 16000

// UnsupportedAudioError marks decode failures that should surface as HTTP 400.
type UnsupportedAudioError struct{ msg string }

func (e *UnsupportedAudioError) Error() string { return e.msg }

func unsupported(format string, args ...any) error {
	return &UnsupportedAudioError{msg: fmt.Sprintf(format, args...)}
}

// DecodeAudio turns an uploaded file's bytes into mono float32 at 16 kHz.
func DecodeAudio(data []byte) ([]float32, error) {
	mono, _, err := DecodeAudioChannels(data)
	return mono, err
}

// DecodeAudioChannels decodes to the 16 kHz mono downmix AND each source channel
// resampled to 16 kHz. Callers that want per-channel information (e.g. an isolated
// microphone on one channel of a stereo recording) use the channels; the mono
// downmix drives diarization and transcription exactly as before. For a mono file
// channels has length 1 and equals mono.
func DecodeAudioChannels(data []byte) (mono []float32, channels [][]float32, err error) {
	var chans [][]float32
	var rate int
	switch {
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WAVE":
		chans, rate, err = decodeWAV(data)
	case looksLikeMP3(data):
		chans, rate, err = decodeMP3(data)
	default:
		head := data
		if len(head) > 8 {
			head = head[:8]
		}
		return nil, nil, unsupported("unrecognized audio container (leading bytes %q) — send WAV or MP3", head)
	}
	if err != nil {
		return nil, nil, err
	}
	channels = make([][]float32, len(chans))
	for i, c := range chans {
		channels[i] = Resample(c, rate, pipelineRate)
	}
	mono = downmix(channels)
	return mono, channels, nil
}

// downmix averages equal-length channel signals into one.
func downmix(channels [][]float32) []float32 {
	if len(channels) == 1 {
		return channels[0]
	}
	n := 0
	for _, c := range channels {
		if len(c) > n {
			n = len(c)
		}
	}
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		var sum float64
		for _, c := range channels {
			if i < len(c) {
				sum += float64(c[i])
			}
		}
		out[i] = float32(sum / float64(len(channels)))
	}
	return out
}

func looksLikeMP3(data []byte) bool {
	if len(data) < 3 {
		return false
	}
	if string(data[:3]) == "ID3" {
		return true
	}
	// MPEG frame sync: 11 set bits, and a valid layer field.
	return data[0] == 0xFF && data[1]&0xE0 == 0xE0 && data[1]&0x06 != 0
}

// decodeWAV returns the source-rate per-channel signals and the sample rate.
func decodeWAV(data []byte) ([][]float32, int, error) {
	// Walk RIFF chunks for fmt and data; other chunks (LIST, fact, ...) are skipped.
	var (
		haveFmt     bool
		format      uint16
		channels    int
		sampleRate  int
		bitsPerSamp int
		chans       [][]float32
		haveData    bool
	)
	pos := 12
	for pos+8 <= len(data) {
		id := string(data[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		body := pos + 8
		if size < 0 || body+size > len(data) {
			// Tolerate a truncated final data chunk — recorders killed mid-write do this.
			if id == "data" {
				size = len(data) - body
			} else {
				break
			}
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return nil, 0, unsupported("WAV fmt chunk too short (%d bytes)", size)
			}
			format = binary.LittleEndian.Uint16(data[body:])
			channels = int(binary.LittleEndian.Uint16(data[body+2:]))
			sampleRate = int(binary.LittleEndian.Uint32(data[body+4:]))
			bitsPerSamp = int(binary.LittleEndian.Uint16(data[body+14:]))
			if format == 0xFFFE && size >= 40 {
				// WAVE_FORMAT_EXTENSIBLE: the real format code leads the SubFormat GUID.
				format = binary.LittleEndian.Uint16(data[body+24:])
			}
			haveFmt = true
		case "data":
			if !haveFmt {
				return nil, 0, unsupported("WAV data chunk before fmt chunk")
			}
			if format == 0x0055 { // MPEG Layer 3 in a RIFF wrapper
				return decodeMP3(data[body : body+size])
			}
			if channels < 1 || sampleRate < 1 {
				return nil, 0, unsupported("WAV has %d channels at %d Hz", channels, sampleRate)
			}
			var err error
			chans, err = pcmToChannels(data[body:body+size], format, bitsPerSamp, channels)
			if err != nil {
				return nil, 0, err
			}
			haveData = true
		}
		pos = body + size
		if size%2 == 1 {
			pos++ // chunks are word-aligned
		}
	}
	if !haveData {
		return nil, 0, unsupported("WAV file has no audio data")
	}
	return chans, sampleRate, nil
}

// pcmToChannels de-interleaves PCM into one float32 slice per channel.
func pcmToChannels(raw []byte, format uint16, bits, channels int) ([][]float32, error) {
	var bytesPer int
	switch {
	case format == 1 && bits == 16:
		bytesPer = 2
	case format == 1 && bits == 24:
		bytesPer = 3
	case format == 1 && bits == 32:
		bytesPer = 4
	case format == 3 && bits == 32:
		bytesPer = 4
	default:
		return nil, unsupported("WAV format %d with %d-bit samples not supported (PCM 16/24/32-bit or float32 expected)", format, bits)
	}
	frame := bytesPer * channels
	n := len(raw) / frame
	out := make([][]float32, channels)
	for c := range out {
		out[c] = make([]float32, n)
	}
	for i := 0; i < n; i++ {
		for c := 0; c < channels; c++ {
			p := raw[i*frame+c*bytesPer:]
			var v float64
			switch {
			case format == 3:
				v = float64(math.Float32frombits(binary.LittleEndian.Uint32(p)))
			case bits == 16:
				v = float64(int16(binary.LittleEndian.Uint16(p))) / 32768
			case bits == 24:
				x := int32(p[0]) | int32(p[1])<<8 | int32(p[2])<<16
				x = x << 8 >> 8 // sign-extend
				v = float64(x) / 8388608
			case bits == 32:
				v = float64(int32(binary.LittleEndian.Uint32(p))) / 2147483648
			}
			out[c][i] = float32(v)
		}
	}
	return out, nil
}

func decodeMP3(data []byte) ([][]float32, int, error) {
	dec, err := mp3.NewDecoder(bytes.NewReader(data))
	if err != nil {
		return nil, 0, unsupported("cannot decode MP3 data: %v", err)
	}
	// go-mp3 always emits 16-bit little-endian stereo at the source sample rate.
	pcm, err := io.ReadAll(dec)
	if err != nil {
		return nil, 0, unsupported("MP3 decode failed: %v", err)
	}
	n := len(pcm) / 4
	left := make([]float32, n)
	right := make([]float32, n)
	for i := 0; i < n; i++ {
		left[i] = float32(int16(binary.LittleEndian.Uint16(pcm[i*4:]))) / 32768
		right[i] = float32(int16(binary.LittleEndian.Uint16(pcm[i*4+2:]))) / 32768
	}
	return [][]float32{left, right}, dec.SampleRate(), nil
}

// Resample converts in from inRate to outRate with a Kaiser-windowed-sinc kernel.
//
// Sample positions are computed with integer arithmetic (n*inRate is exact in int64;
// float64 only ever holds the sub-sample fraction). This matters: the Python service
// once resampled with float32 source coordinates, which are exact only to 2^24 —
// audio past ~6 minutes at 48 kHz was silently corrupted (20.4 dB SNR falling to
// 6.3 dB) and a speaker scoring 0.56 exact scored 0.14 corrupted. Positions here
// stay exact for any recording length.
func Resample(in []float32, inRate, outRate int) []float32 {
	if inRate == outRate || len(in) == 0 {
		return in
	}
	const (
		halfTaps = 16  // 32-tap kernel
		phases   = 512 // fractional-position resolution of the precomputed kernel
	)
	// Cutoff at 90% of the narrower Nyquist, in cycles per input sample.
	fc := 0.45
	if outRate < inRate {
		fc = 0.45 * float64(outRate) / float64(inRate)
	}
	// Precompute the Kaiser-windowed-sinc kernel per fractional phase, each row
	// normalized to unity DC gain. Per-sample work is then 32 multiply-adds.
	const beta = 8.0
	i0beta := besselI0(beta)
	table := make([][2 * halfTaps]float64, phases+1)
	for p := 0; p <= phases; p++ {
		frac := float64(p) / phases
		var norm float64
		for k := -halfTaps + 1; k <= halfTaps; k++ {
			t := float64(k) - frac
			x := t / halfTaps
			if x < -1 || x > 1 {
				continue
			}
			w := besselI0(beta*math.Sqrt(1-x*x)) / i0beta
			table[p][k+halfTaps-1] = 2 * fc * sinc(2*fc*t) * w
			norm += table[p][k+halfTaps-1]
		}
		for k := range table[p] {
			table[p][k] /= norm
		}
	}

	outLen := int(int64(len(in)) * int64(outRate) / int64(inRate))
	out := make([]float32, outLen)
	for n := 0; n < outLen; n++ {
		num := int64(n) * int64(inRate)
		idx := int(num / int64(outRate))
		frac := float64(num%int64(outRate)) / float64(outRate)
		row := &table[int(frac*phases+0.5)]
		lo, hi := idx-halfTaps+1, idx+halfTaps
		var acc float64
		if lo >= 0 && hi < len(in) {
			for k, c := range row {
				acc += float64(in[lo+k]) * c
			}
		} else {
			var norm float64 // renormalize at the clipped edges
			for k, c := range row {
				if j := lo + k; j >= 0 && j < len(in) {
					acc += float64(in[j]) * c
					norm += c
				}
			}
			if norm != 0 {
				acc /= norm
			}
		}
		out[n] = float32(acc)
	}
	return out
}

func sinc(x float64) float64 {
	if x == 0 {
		return 1
	}
	return math.Sin(math.Pi*x) / (math.Pi * x)
}

// besselI0 is the zeroth-order modified Bessel function (for the Kaiser window).
func besselI0(x float64) float64 {
	sum, term := 1.0, 1.0
	half := x / 2
	for k := 1; k < 32; k++ {
		term *= (half / float64(k)) * (half / float64(k))
		sum += term
		if term < 1e-12*sum {
			break
		}
	}
	return sum
}
