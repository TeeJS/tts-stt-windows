// diar-enroll computes a speaker's voice profile from a 16 kHz mono WAV with the
// sherpa-onnx embedding model and saves it as <name>.npy — the same whole-clip
// procedure the Python service's /enroll uses, for rebuilding profiles offline.
//
// Usage:
//
//	diar-enroll -embedding model.onnx -enrollments <dir> -wav clip.wav -name "T.J. Schmitz"
//
// Needs the sherpa DLLs on PATH.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"github.com/TeeJS/tts-stt-windows/internal/diarize"
)

func main() {
	model := flag.String("embedding", "", "path to the speaker-embedding .onnx model (required)")
	wav := flag.String("wav", "", "16 kHz mono WAV of the speaker (required)")
	name := flag.String("name", "", "speaker name, becomes the profile filename (required)")
	enrollments := flag.String("enrollments", "", "profile directory (required)")
	threads := flag.Int("threads", runtime.NumCPU()/2, "onnxruntime threads")
	start := flag.Float64("start", 0, "seconds into the clip to start")
	dur := flag.Float64("dur", 0, "seconds of the clip to use (0 = to end)")
	flag.Parse()
	if *model == "" || *wav == "" || *name == "" || *enrollments == "" {
		flag.Usage()
		os.Exit(2)
	}

	wave := sherpa.ReadWave(*wav)
	if wave == nil {
		log.Fatalf("cannot read %s (must be a PCM WAV)", *wav)
	}
	if wave.SampleRate != 16000 {
		log.Fatalf("clip must be 16 kHz (got %d Hz) — convert first", wave.SampleRate)
	}
	if *start > 0 || *dur > 0 {
		lo := min(int(*start*16000), len(wave.Samples))
		hi := len(wave.Samples)
		if *dur > 0 {
			hi = min(lo+int(*dur*16000), hi)
		}
		wave.Samples = wave.Samples[lo:hi]
	}

	ex := sherpa.NewSpeakerEmbeddingExtractor(&sherpa.SpeakerEmbeddingExtractorConfig{
		Model: *model, NumThreads: *threads, Provider: "cpu",
	})
	if ex == nil {
		log.Fatalf("failed to load embedding model %s", *model)
	}
	defer sherpa.DeleteSpeakerEmbeddingExtractor(ex)

	stream := ex.CreateStream()
	defer sherpa.DeleteOnlineStream(stream)
	stream.AcceptWaveform(wave.SampleRate, wave.Samples)
	stream.InputFinished()
	if !ex.IsReady(stream) {
		log.Fatal("clip too short for an embedding")
	}
	emb := ex.Compute(stream)

	store, err := diarize.NewEnrollmentStore(*enrollments)
	if err != nil {
		log.Fatal(err)
	}
	if err := store.Save(*name, emb); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("enrolled %q (%.1fs of audio, dim %d)\n",
		*name, float64(len(wave.Samples))/float64(wave.SampleRate), len(emb))
}
