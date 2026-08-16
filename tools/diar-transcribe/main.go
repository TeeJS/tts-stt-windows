// diar-transcribe runs the complete meeting pipeline offline — decode, diarize,
// transcribe, attribute — and prints the same JSON body POST /transcribe will
// return. The offline equivalent of the service's /transcribe for validation.
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
	"github.com/TeeJS/tts-stt-windows/internal/engine"
)

func main() {
	seg := flag.String("seg", "", "pyannote segmentation model.onnx (required)")
	embed := flag.String("embedding", "", "speaker-embedding .onnx (required)")
	stt := flag.String("stt", "", "transducer STT model dir, e.g. parakeet (required)")
	enrollments := flag.String("enrollments", "", "directory of .npy profiles (required)")
	wav := flag.String("wav", "", "recording to process (required)")
	threshold := flag.Float64("threshold", diarize.SimilarityThreshold, "identification threshold")
	clusterThreshold := flag.Float64("cluster-threshold", 0.5, "sherpa clustering threshold")
	attendees := flag.String("attendees", "", "comma-separated attendee names (optional)")
	meName := flag.String("me", "", "channel-guided ID: name the speaker isolated on the mic channel")
	meChannel := flag.Int("me-channel", 0, "which channel is the mic (0=left, 1=right)")
	threads := flag.Int("threads", runtime.NumCPU()/2, "onnxruntime threads")
	flag.Parse()
	if *seg == "" || *embed == "" || *stt == "" || *enrollments == "" || *wav == "" {
		flag.Usage()
		os.Exit(2)
	}

	data, err := os.ReadFile(*wav)
	if err != nil {
		log.Fatal(err)
	}
	samples, channels, err := diarize.DecodeAudioChannels(data)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Fprintf(os.Stderr, "decoded %.1fs of audio (%d channel(s))\n", float64(len(samples))/16000, len(channels))

	var me *diarize.MeHint
	if *meName != "" {
		me = &diarize.MeHint{Name: *meName, Channel: *meChannel, Signals: channels}
	}

	store, err := diarize.NewEnrollmentStore(*enrollments)
	if err != nil {
		log.Fatal(err)
	}
	d, err := diarize.NewDiarizer(*seg, *embed, *threads, float32(*clusterThreshold), store)
	if err != nil {
		log.Fatal(err)
	}
	defer d.Close()
	rec, err := engine.NewTransducerSTT(*stt, *threads)
	if err != nil {
		log.Fatal(err)
	}
	defer rec.Close()

	var att []string
	for _, a := range strings.Split(*attendees, ",") {
		if a = strings.TrimSpace(a); a != "" {
			att = append(att, a)
		}
	}

	segments, report := d.Transcribe(rec, samples, *threshold, att, me)
	out, _ := json.MarshalIndent(struct {
		SpeakerReport *diarize.SpeakerReport `json:"speaker_report"`
		Segments      []diarize.Segment      `json:"segments"`
	}{report, segments}, "", "  ")
	fmt.Println(string(out))
}
