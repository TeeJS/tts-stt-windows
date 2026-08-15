// tok-probe: dump raw token timestamps from a recognizer for a slice of audio.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/TeeJS/tts-stt-windows/internal/diarize"
	"github.com/TeeJS/tts-stt-windows/internal/engine"
)

func main() {
	stt := flag.String("stt", "", "transducer model dir")
	wav := flag.String("wav", "", "recording")
	start := flag.Float64("start", 0, "slice start sec")
	dur := flag.Float64("dur", 20, "slice duration sec")
	flag.Parse()

	data, err := os.ReadFile(*wav)
	if err != nil {
		log.Fatal(err)
	}
	samples, err := diarize.DecodeAudio(data)
	if err != nil {
		log.Fatal(err)
	}
	lo, hi := int(*start*16000), int((*start+*dur)*16000)
	if hi > len(samples) {
		hi = len(samples)
	}
	rec, err := engine.NewTransducerSTT(*stt, 4)
	if err != nil {
		log.Fatal(err)
	}
	defer rec.Close()
	text, toks := rec.TranscribeTokens(samples[lo:hi], 16000)
	fmt.Printf("text: %s\n", text)
	fmt.Printf("tokens: %d\n", len(toks))
	for i, t := range toks {
		if i < 25 || i > len(toks)-5 {
			fmt.Printf("  %3d %-12q %7.3f - %7.3f\n", i, t.Text, t.Start, t.End)
		}
	}
}
