// diar-identify runs the full diarization + speaker-identification pipeline on a
// recording and prints the speaker report as JSON — the offline equivalent of the
// service's POST /identify, used for A/B comparison against the Python service.
//
// Needs the sherpa DLLs on PATH.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"
	"strings"

	"github.com/TeeJS/tts-stt-windows/internal/diarize"
)

func main() {
	seg := flag.String("seg", "", "pyannote segmentation model.onnx (required)")
	embed := flag.String("embedding", "", "speaker-embedding .onnx (required)")
	enrollments := flag.String("enrollments", "", "directory of .npy profiles (required)")
	wav := flag.String("wav", "", "recording to process (required; WAV or MP3-in-WAV)")
	threshold := flag.Float64("threshold", diarize.SimilarityThreshold, "identification threshold")
	clusterThreshold := flag.Float64("cluster-threshold", 0.5, "sherpa clustering threshold")
	attendees := flag.String("attendees", "", "comma-separated attendee names (optional)")
	threads := flag.Int("threads", runtime.NumCPU()/2, "onnxruntime threads")
	flag.Parse()
	if *seg == "" || *embed == "" || *enrollments == "" || *wav == "" {
		flag.Usage()
		os.Exit(2)
	}

	data, err := os.ReadFile(*wav)
	if err != nil {
		log.Fatal(err)
	}
	samples, err := diarize.DecodeAudio(data)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Fprintf(os.Stderr, "decoded %.1fs of audio\n", float64(len(samples))/16000)

	store, err := diarize.NewEnrollmentStore(*enrollments)
	if err != nil {
		log.Fatal(err)
	}
	d, err := diarize.NewDiarizer(*seg, *embed, *threads, float32(*clusterThreshold), store)
	if err != nil {
		log.Fatal(err)
	}
	defer d.Close()

	var att []string
	for _, a := range strings.Split(*attendees, ",") {
		if a = strings.TrimSpace(a); a != "" {
			att = append(att, a)
		}
	}

	report := d.IdentifySpeakers(samples, *threshold, att, nil)
	out, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(out))
}
