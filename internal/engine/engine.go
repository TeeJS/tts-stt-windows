// Package engine wraps sherpa-onnx (github.com/k2-fsa/sherpa-onnx-go) behind the two tiny
// interfaces the Wyoming server needs. One engine instance each for STT and TTS, mutex-serialized:
// sherpa contexts are not documented thread-safe, and speech requests are inherently serial anyway.
//
// Ships with the exe (from sherpa-onnx-go-windows/lib/x86_64-pc-windows-gnu):
// onnxruntime.dll, sherpa-onnx-c-api.dll, sherpa-onnx-cxx-api.dll.
package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

// TTS synthesizes text to 16-bit mono PCM at the returned sample rate.
type TTS interface {
	Synthesize(text string) (pcm []byte, sampleRate int, err error)
}

// STT transcribes one complete utterance of 16-bit mono PCM at the given sample rate.
type STT interface {
	Transcribe(pcm []byte, sampleRate int) (string, error)
}

// ---- Piper voice TTS (VITS via sherpa-onnx) ----

type PiperTTS struct {
	mu  sync.Mutex
	tts *sherpa.OfflineTts
}

// NewPiperTTS loads a piper voice directory as distributed in sherpa-onnx's tts-models releases
// (vits-piper-*: <name>.onnx + tokens.txt + espeak-ng-data/). The .onnx file is auto-located so
// voice directories can be dropped in without config listing the model filename.
func NewPiperTTS(voiceDir string, numThreads int) (*PiperTTS, error) {
	model, err := findOne(voiceDir, "*.onnx")
	if err != nil {
		return nil, fmt.Errorf("piper voice %s: %w", voiceDir, err)
	}
	cfg := sherpa.OfflineTtsConfig{
		Model: sherpa.OfflineTtsModelConfig{
			Vits: sherpa.OfflineTtsVitsModelConfig{
				Model:       model,
				Tokens:      filepath.Join(voiceDir, "tokens.txt"),
				DataDir:     filepath.Join(voiceDir, "espeak-ng-data"),
				NoiseScale:  0.667, // sherpa-onnx documented defaults for VITS
				NoiseScaleW: 0.8,
				LengthScale: 1.0,
			},
			NumThreads: numThreads,
			Provider:   "cpu",
		},
	}
	tts := sherpa.NewOfflineTts(&cfg)
	if tts == nil {
		return nil, fmt.Errorf("piper voice %s: sherpa-onnx failed to load (see stderr for model errors)", voiceDir)
	}
	return &PiperTTS{tts: tts}, nil
}

func (p *PiperTTS) Synthesize(text string) ([]byte, int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	audio := p.tts.Generate(text, 0, 1.0)
	if audio == nil || len(audio.Samples) == 0 {
		return nil, 0, fmt.Errorf("synthesis produced no audio for %q", text)
	}
	return floatToPCM16(audio.Samples), audio.SampleRate, nil
}

func (p *PiperTTS) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tts != nil {
		sherpa.DeleteOfflineTts(p.tts)
		p.tts = nil
	}
}

// ---- Speech-to-text (whisper or parakeet/transducer via sherpa-onnx) ----

type Recognizer struct {
	mu  sync.Mutex
	rec *sherpa.OfflineRecognizer
}

// NewWhisperSTT loads a whisper model directory as distributed in sherpa-onnx's asr-models
// releases (sherpa-onnx-whisper-*: *-encoder.int8.onnx, *-decoder.int8.onnx, *-tokens.txt).
func NewWhisperSTT(modelDir, language string, numThreads int) (*Recognizer, error) {
	// sherpa-onnx whisper archives carry BOTH fp32 and int8 variants of each model file;
	// prefer int8 (much faster on CPU, negligible quality cost for dictation).
	encoder, err := findPreferred(modelDir, "*encoder*.int8.onnx", "*encoder*.onnx")
	if err != nil {
		return nil, fmt.Errorf("whisper %s: %w", modelDir, err)
	}
	decoder, err := findPreferred(modelDir, "*decoder*.int8.onnx", "*decoder*.onnx")
	if err != nil {
		return nil, fmt.Errorf("whisper %s: %w", modelDir, err)
	}
	tokens, err := findOne(modelDir, "*tokens*.txt")
	if err != nil {
		return nil, fmt.Errorf("whisper %s: %w", modelDir, err)
	}
	cfg := sherpa.OfflineRecognizerConfig{
		FeatConfig: sherpa.FeatureConfig{SampleRate: 16000, FeatureDim: 80},
		ModelConfig: sherpa.OfflineModelConfig{
			Whisper:    sherpa.OfflineWhisperModelConfig{Encoder: encoder, Decoder: decoder, Language: language, Task: "transcribe"},
			Tokens:     tokens,
			NumThreads: numThreads,
			Provider:   "cpu",
			ModelType:  "whisper",
		},
		DecodingMethod: "greedy_search",
	}
	return newRecognizer(&cfg, "whisper", modelDir)
}

// NewTransducerSTT loads a NeMo transducer model directory (e.g. parakeet:
// encoder.int8.onnx / decoder.int8.onnx / joiner.int8.onnx / tokens.txt).
func NewTransducerSTT(modelDir string, numThreads int) (*Recognizer, error) {
	encoder, err := findPreferred(modelDir, "encoder*.int8.onnx", "encoder*.onnx")
	if err != nil {
		return nil, fmt.Errorf("transducer %s: %w", modelDir, err)
	}
	decoder, err := findPreferred(modelDir, "decoder*.int8.onnx", "decoder*.onnx")
	if err != nil {
		return nil, fmt.Errorf("transducer %s: %w", modelDir, err)
	}
	joiner, err := findPreferred(modelDir, "joiner*.int8.onnx", "joiner*.onnx")
	if err != nil {
		return nil, fmt.Errorf("transducer %s: %w", modelDir, err)
	}
	cfg := sherpa.OfflineRecognizerConfig{
		FeatConfig: sherpa.FeatureConfig{SampleRate: 16000, FeatureDim: 80},
		ModelConfig: sherpa.OfflineModelConfig{
			Transducer: sherpa.OfflineTransducerModelConfig{Encoder: encoder, Decoder: decoder, Joiner: joiner},
			Tokens:     filepath.Join(modelDir, "tokens.txt"),
			NumThreads: numThreads,
			Provider:   "cpu",
			ModelType:  "nemo_transducer",
		},
		DecodingMethod: "greedy_search",
	}
	return newRecognizer(&cfg, "transducer", modelDir)
}

func newRecognizer(cfg *sherpa.OfflineRecognizerConfig, kind, dir string) (*Recognizer, error) {
	rec := sherpa.NewOfflineRecognizer(cfg)
	if rec == nil {
		return nil, fmt.Errorf("%s %s: sherpa-onnx failed to load (see stderr for model errors)", kind, dir)
	}
	return &Recognizer{rec: rec}, nil
}

func (r *Recognizer) Transcribe(pcm []byte, sampleRate int) (string, error) {
	samples := pcm16ToFloat(pcm)
	if len(samples) == 0 {
		return "", nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	stream := sherpa.NewOfflineStream(r.rec)
	defer sherpa.DeleteOfflineStream(stream)
	stream.AcceptWaveform(sampleRate, samples) // sherpa resamples to the model's rate internally
	r.rec.Decode(stream)
	return stream.GetResult().Text, nil
}

func (r *Recognizer) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rec != nil {
		sherpa.DeleteOfflineRecognizer(r.rec)
		r.rec = nil
	}
}

// ---- PCM conversions (16-bit little-endian mono <-> float32) ----

func floatToPCM16(samples []float32) []byte {
	out := make([]byte, len(samples)*2)
	for i, s := range samples {
		if s > 1 {
			s = 1
		} else if s < -1 {
			s = -1
		}
		v := int16(s * 32767)
		out[i*2] = byte(v)
		out[i*2+1] = byte(v >> 8)
	}
	return out
}

func pcm16ToFloat(pcm []byte) []float32 {
	n := len(pcm) / 2
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		v := int16(uint16(pcm[i*2]) | uint16(pcm[i*2+1])<<8)
		out[i] = float32(v) / 32768
	}
	return out
}

// findPreferred tries patterns in order and returns the single match of the first pattern that
// matches anything — e.g. int8 model files ahead of their fp32 siblings.
func findPreferred(dir string, patterns ...string) (string, error) {
	var lastErr error
	for _, p := range patterns {
		f, err := findOne(dir, p)
		if err == nil {
			return f, nil
		}
		lastErr = err
	}
	return "", lastErr
}

// findOne returns the single file matching pattern in dir, erroring on zero or ambiguity —
// model directories are machine-managed, so ambiguity means a broken download, not a choice.
func findOne(dir, pattern string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		if _, statErr := os.Stat(dir); statErr != nil {
			return "", fmt.Errorf("model directory missing: %w", statErr)
		}
		return "", fmt.Errorf("no %s in %s", pattern, dir)
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("multiple %s in %s", pattern, dir)
	}
	return matches[0], nil
}
