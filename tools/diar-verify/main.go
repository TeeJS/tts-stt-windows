// diar-verify is a dev-only probe for the milestone-2 compatibility gate: it embeds a
// known speaker's clip with the sherpa-onnx wespeaker model and prints cosine scores
// against every existing .npy enrollment profile, i.e. exactly what /identify will do
// later. Run it against profiles written by the Python (pyannote) stack to decide
// whether they transfer or speakers must be re-enrolled.
//
// Usage:
//
//	diar-verify -embedding wespeaker_en_voxceleb_resnet34_LM.onnx -clip tj_16k_mono.wav -enrollments <dir>
//
// Needs the sherpa DLLs on PATH (e.g. the repo's dist\ folder).
package main

import (
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"github.com/TeeJS/tts-stt-windows/internal/diarize"
)

func cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return math.NaN()
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	return dot / (math.Sqrt(na)*math.Sqrt(nb) + 1e-8)
}

func main() {
	model := flag.String("embedding", "", "path to the speaker-embedding .onnx model (required)")
	clip := flag.String("clip", "", "16 kHz mono WAV of a known speaker (required)")
	enrollments := flag.String("enrollments", "", "directory of .npy profiles (required)")
	threads := flag.Int("threads", runtime.NumCPU()/2, "onnxruntime threads")
	start := flag.Float64("start", 0, "seconds into the clip to start")
	dur := flag.Float64("dur", 0, "seconds of the clip to use (0 = to end)")
	flag.Parse()
	if *model == "" || *clip == "" || *enrollments == "" {
		flag.Usage()
		os.Exit(2)
	}

	wave := sherpa.ReadWave(*clip)
	if wave == nil {
		log.Fatalf("cannot read %s (must be a PCM WAV)", *clip)
	}
	fmt.Printf("clip: %s (%.1fs @ %d Hz)\n", *clip, float64(len(wave.Samples))/float64(wave.SampleRate), wave.SampleRate)
	if wave.SampleRate != 16000 {
		log.Fatalf("clip must be 16 kHz mono (got %d Hz) — convert first", wave.SampleRate)
	}
	if *start > 0 || *dur > 0 {
		lo := min(int(*start*16000), len(wave.Samples))
		hi := len(wave.Samples)
		if *dur > 0 {
			hi = min(lo+int(*dur*16000), hi)
		}
		wave.Samples = wave.Samples[lo:hi]
		fmt.Printf("using %.1fs-%.1fs (%.1fs)\n", *start, float64(hi)/16000, float64(hi-lo)/16000)
	}

	ex := sherpa.NewSpeakerEmbeddingExtractor(&sherpa.SpeakerEmbeddingExtractorConfig{
		Model: *model, NumThreads: *threads, Provider: "cpu",
	})
	if ex == nil {
		log.Fatalf("failed to load embedding model %s", *model)
	}
	defer sherpa.DeleteSpeakerEmbeddingExtractor(ex)
	fmt.Printf("model: %s (dim %d)\n\n", filepath.Base(*model), ex.Dim())

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
	profiles, err := store.AllEmbeddings()
	if err != nil {
		log.Fatal(err)
	}
	if len(profiles) == 0 {
		log.Fatalf("no readable .npy profiles in %s", *enrollments)
	}

	type row struct {
		name  string
		score float64
		dim   int
	}
	var rows []row
	for name, prof := range profiles {
		rows = append(rows, row{name, cosine(emb, prof), len(prof)})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].score > rows[j].score })

	fmt.Printf("%-30s %8s %6s\n", "speaker", "cosine", "dim")
	for _, r := range rows {
		note := ""
		if r.dim != ex.Dim() {
			note = "  (DIM MISMATCH)"
		}
		fmt.Printf("%-30s %8.4f %6d%s\n", r.name, r.score, r.dim, note)
	}
	fmt.Printf("\nGate: the clip's true speaker should score >= 0.70 with clear margin over everyone else.\n")
}
