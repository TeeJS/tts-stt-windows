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

// ---- VITS voice TTS (Piper and Coqui, both via sherpa-onnx) ----

type PiperTTS struct {
	mu    sync.Mutex
	tts   *sherpa.OfflineTts
	speed float32
}

// NewPiperTTS loads a VITS voice directory. It handles both families the catalog offers, which
// differ in how they turn text into phonemes:
//   - Piper (vits-piper-*): <voice>.onnx + tokens.txt + espeak-ng-data/ (phonemized by espeak)
//   - Coqui (vits-coqui-*): model.onnx + tokens.txt + often lexicon.txt, no espeak data
//
// Paths are discovered rather than configured, so a voice directory can simply be dropped in.
// Setting DataDir to a directory that doesn't exist makes sherpa fail to load, so each optional
// input is only passed when actually present.
func NewPiperTTS(voiceDir string, numThreads int) (*PiperTTS, error) {
	model, err := findPreferred(voiceDir, "*.int8.onnx", "*.onnx")
	if err != nil {
		return nil, fmt.Errorf("voice %s: %w", voiceDir, err)
	}
	vits := sherpa.OfflineTtsVitsModelConfig{
		Model:       model,
		Tokens:      filepath.Join(voiceDir, "tokens.txt"),
		NoiseScale:  0.667, // sherpa-onnx documented defaults for VITS
		NoiseScaleW: 0.8,
		LengthScale: 1.0,
	}
	if d := filepath.Join(voiceDir, "espeak-ng-data"); isDir(d) {
		vits.DataDir = d
	}
	if f := filepath.Join(voiceDir, "lexicon.txt"); isFile(f) {
		vits.Lexicon = f
	}
	if vits.DataDir == "" && vits.Lexicon == "" {
		return nil, fmt.Errorf("voice %s: neither espeak-ng-data nor lexicon.txt found — incomplete download?", voiceDir)
	}
	cfg := sherpa.OfflineTtsConfig{
		Model: sherpa.OfflineTtsModelConfig{
			Vits:       vits,
			NumThreads: numThreads,
			Provider:   "cpu",
		},
	}
	tts := sherpa.NewOfflineTts(&cfg)
	if tts == nil {
		return nil, fmt.Errorf("voice %s: sherpa-onnx failed to load (see log for model errors)", voiceDir)
	}
	return &PiperTTS{tts: tts, speed: 1.0}, nil
}

// SetSpeed adjusts speaking rate (1.0 = the voice's natural pace; higher is faster).
func (p *PiperTTS) SetSpeed(speed float32) {
	if speed < 0.5 || speed > 2.0 {
		return // outside this range the voice distorts badly; ignore rather than produce garbage
	}
	p.mu.Lock()
	p.speed = speed
	p.mu.Unlock()
}

func (p *PiperTTS) Synthesize(text string) ([]byte, int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	audio := p.tts.Generate(text, 0, p.speed)
	if audio == nil || len(audio.Samples) == 0 {
		return nil, 0, fmt.Errorf("synthesis produced no audio for %q", text)
	}
	return floatToPCM16(audio.Samples), audio.SampleRate, nil
}

// Close is safe on a nil receiver: callers hand it the previously-active engine, which is nil on
// the first load, and a nil pointer boxed in an interface is not itself nil.
func (p *PiperTTS) Close() {
	if p == nil {
		return
	}
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

// NewWhisperTranslate loads a whisper model in "translate" mode: it emits ENGLISH from any spoken
// language (sherpa's Task:"translate"; whisper-only). Language is left empty so the source language
// is auto-detected. Used by the dedicated translate STT endpoint (see cmd/tts-sst/translate.go).
func NewWhisperTranslate(modelDir string, numThreads int) (*Recognizer, error) {
	return NewWhisperSTT(modelDir, "", "translate", numThreads)
}

// NewWhisperSTT loads a whisper model directory as distributed in sherpa-onnx's asr-models
// releases (sherpa-onnx-whisper-*: *-encoder.int8.onnx, *-decoder.int8.onnx, *-tokens.txt).
// `task` is sherpa's whisper task: "transcribe" (same-language text) or "translate" (English out).
func NewWhisperSTT(modelDir, language, task string, numThreads int) (*Recognizer, error) {
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
			Whisper:    sherpa.OfflineWhisperModelConfig{Encoder: encoder, Decoder: decoder, Language: language, Task: task},
			Tokens:     tokens,
			NumThreads: numThreads,
			Provider:   "cpu",
			ModelType:  "whisper",
		},
		DecodingMethod: "greedy_search",
	}
	return newRecognizer(&cfg, "whisper", modelDir)
}

// NewSTT loads whichever speech model lives in modelDir. `family` comes from the catalog entry;
// an empty family falls back to sniffing the directory contents, so a model dropped in by hand
// still loads.
func NewSTT(modelDir, family, language string, numThreads int) (*Recognizer, error) {
	if family == "" {
		family = sniffFamily(modelDir)
	}
	switch family {
	case "whisper":
		return NewWhisperSTT(modelDir, language, "transcribe", numThreads)
	case "parakeet":
		return NewTransducerSTT(modelDir, numThreads)
	case "sense-voice":
		return NewSenseVoiceSTT(modelDir, language, numThreads)
	case "moonshine":
		return NewMoonshineSTT(modelDir, numThreads)
	case "dolphin":
		return NewDolphinSTT(modelDir, numThreads)
	default:
		return nil, fmt.Errorf("unsupported speech model family %q in %s", family, modelDir)
	}
}

// sniffFamily identifies a model directory by its distinctive files, for models installed
// outside the catalog.
func sniffFamily(dir string) string {
	has := func(pattern string) bool {
		m, _ := filepath.Glob(filepath.Join(dir, pattern))
		return len(m) > 0
	}
	switch {
	case has("*encoder*.onnx") && has("*decoder*.onnx") && !has("joiner*.onnx") && has("preprocess*.onnx"):
		return "moonshine"
	case has("joiner*.onnx"):
		return "parakeet"
	case has("*encoder*.onnx") && has("*decoder*.onnx"):
		return "whisper"
	case has("model.int8.onnx") || has("model.onnx"):
		return "sense-voice" // also matches dolphin; both are single-file CTC models
	}
	return ""
}

// NewSenseVoiceSTT loads a SenseVoice model (single model.onnx + tokens.txt), which recognizes
// Chinese, English, Japanese, Korean and Cantonese in one model.
func NewSenseVoiceSTT(modelDir, language string, numThreads int) (*Recognizer, error) {
	model, err := findPreferred(modelDir, "model.int8.onnx", "model.onnx", "*.int8.onnx", "*.onnx")
	if err != nil {
		return nil, fmt.Errorf("sense-voice %s: %w", modelDir, err)
	}
	cfg := sherpa.OfflineRecognizerConfig{
		FeatConfig: sherpa.FeatureConfig{SampleRate: 16000, FeatureDim: 80},
		ModelConfig: sherpa.OfflineModelConfig{
			// Empty Language means auto-detect among the five it supports.
			SenseVoice: sherpa.OfflineSenseVoiceModelConfig{Model: model, Language: language, UseInverseTextNormalization: 1},
			Tokens:     filepath.Join(modelDir, "tokens.txt"),
			NumThreads: numThreads,
			Provider:   "cpu",
		},
		DecodingMethod: "greedy_search",
	}
	return newRecognizer(&cfg, "sense-voice", modelDir)
}

// NewMoonshineSTT loads a Moonshine model (preprocessor + encoder + cached/uncached decoders).
func NewMoonshineSTT(modelDir string, numThreads int) (*Recognizer, error) {
	pre, err := findPreferred(modelDir, "preprocess*.int8.onnx", "preprocess*.onnx")
	if err != nil {
		return nil, fmt.Errorf("moonshine %s: %w", modelDir, err)
	}
	enc, err := findPreferred(modelDir, "encode*.int8.onnx", "encode*.onnx")
	if err != nil {
		return nil, fmt.Errorf("moonshine %s: %w", modelDir, err)
	}
	uncached, err := findPreferred(modelDir, "uncached_decode*.int8.onnx", "uncached_decode*.onnx")
	if err != nil {
		return nil, fmt.Errorf("moonshine %s: %w", modelDir, err)
	}
	cached, err := findPreferred(modelDir, "cached_decode*.int8.onnx", "cached_decode*.onnx")
	if err != nil {
		return nil, fmt.Errorf("moonshine %s: %w", modelDir, err)
	}
	cfg := sherpa.OfflineRecognizerConfig{
		FeatConfig: sherpa.FeatureConfig{SampleRate: 16000, FeatureDim: 80},
		ModelConfig: sherpa.OfflineModelConfig{
			Moonshine: sherpa.OfflineMoonshineModelConfig{
				Preprocessor: pre, Encoder: enc, UncachedDecoder: uncached, CachedDecoder: cached,
			},
			Tokens:     filepath.Join(modelDir, "tokens.txt"),
			NumThreads: numThreads,
			Provider:   "cpu",
		},
		DecodingMethod: "greedy_search",
	}
	return newRecognizer(&cfg, "moonshine", modelDir)
}

// NewDolphinSTT loads a Dolphin CTC model (single model.onnx + tokens.txt) covering 40 Asian
// languages.
func NewDolphinSTT(modelDir string, numThreads int) (*Recognizer, error) {
	model, err := findPreferred(modelDir, "model.int8.onnx", "model.onnx", "*.int8.onnx", "*.onnx")
	if err != nil {
		return nil, fmt.Errorf("dolphin %s: %w", modelDir, err)
	}
	cfg := sherpa.OfflineRecognizerConfig{
		FeatConfig: sherpa.FeatureConfig{SampleRate: 16000, FeatureDim: 80},
		ModelConfig: sherpa.OfflineModelConfig{
			Dolphin:    sherpa.OfflineDolphinModelConfig{Model: model},
			Tokens:     filepath.Join(modelDir, "tokens.txt"),
			NumThreads: numThreads,
			Provider:   "cpu",
		},
		DecodingMethod: "greedy_search",
	}
	return newRecognizer(&cfg, "dolphin", modelDir)
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

// Token is one decoded subword with its timing (seconds from the start of the
// audio passed in). End is zero when the model emits no durations.
type Token struct {
	Text       string
	Start, End float32
}

// TranscribeTokens is Transcribe plus per-token timestamps, for callers that need
// word-level timing (meeting diarization). Not every model family produces
// timestamps — the returned slice is empty when this one doesn't, and callers
// must fall back to timing-free handling.
func (r *Recognizer) TranscribeTokens(samples []float32, sampleRate int) (string, []Token) {
	if len(samples) == 0 {
		return "", nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rec == nil { // closed underneath us (engine swapped mid-job)
		return "", nil
	}
	stream := sherpa.NewOfflineStream(r.rec)
	defer sherpa.DeleteOfflineStream(stream)
	stream.AcceptWaveform(sampleRate, samples)
	r.rec.Decode(stream)
	res := stream.GetResult()
	if len(res.Timestamps) != len(res.Tokens) {
		return res.Text, nil
	}
	toks := make([]Token, len(res.Tokens))
	for i := range res.Tokens {
		t := Token{Text: res.Tokens[i], Start: res.Timestamps[i]}
		switch {
		case i < len(res.Durations) && res.Durations[i] > 0:
			t.End = t.Start + res.Durations[i]
		case i+1 < len(res.Timestamps):
			t.End = res.Timestamps[i+1]
		default:
			t.End = t.Start
		}
		toks[i] = t
	}
	return res.Text, toks
}

// Close is safe on a nil receiver — see PiperTTS.Close.
func (r *Recognizer) Close() {
	if r == nil {
		return
	}
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

func isDir(p string) bool  { fi, err := os.Stat(p); return err == nil && fi.IsDir() }
func isFile(p string) bool { fi, err := os.Stat(p); return err == nil && !fi.IsDir() }

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
