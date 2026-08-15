package diarize

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/TeeJS/tts-stt-windows/internal/engine"
)

func TestSpeechWindows(t *testing.T) {
	timeline := []Turn{
		{Start: 1, End: 20, Label: 0},
		{Start: 22, End: 45, Label: 1},  // 1..45 fits in 50s
		{Start: 60, End: 100, Label: 0}, // adding this would exceed -> cut at the gap
		{Start: 100.5, End: 105, Label: 1},
	}
	got := speechWindows(timeline, 200)
	want := [][2]float64{{1, 45}, {60, 105}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestSpeechWindowsHardSplit(t *testing.T) {
	// One 120s turn with no gaps: split at 50/50/20.
	got := speechWindows([]Turn{{Start: 0, End: 120, Label: 0}}, 120)
	want := [][2]float64{{0, 50}, {50, 100}, {100, 120}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestTokensToWords(t *testing.T) {
	toks := []engine.Token{ // values chosen to be exact in float32
		{Text: "▁hel", Start: 0.5, End: 0.625},
		{Text: "lo", Start: 0.625, End: 0.75},
		{Text: "▁world", Start: 1.0, End: 1.5},
		{Text: " Go", Start: 2.0, End: 2.25}, // parakeet-style: leading space, not ▁
		{Text: "od", Start: 2.25, End: 2.5},
		{Text: ".", Start: 2.5, End: 2.75}, // punctuation glues to the previous word
	}
	got := tokensToWords(toks, 10.0)
	want := []Word{
		{Text: " hello", Start: 10.5, End: 10.75},
		{Text: " world", Start: 11.0, End: 11.5},
		{Text: " Good.", Start: 12.0, End: 12.75},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// fakeRecognizer emits one token per second of audio, or plain text with no
// tokens when timestamps are "unsupported".
type fakeRecognizer struct {
	timestamps bool
	calls      [][2]int // sample offsets seen, for window verification
}

func (f *fakeRecognizer) TranscribeTokens(samples []float32, rate int) (string, []engine.Token) {
	f.calls = append(f.calls, [2]int{len(samples), rate})
	sec := len(samples) / rate
	if !f.timestamps {
		return "turn text here", nil
	}
	var toks []engine.Token
	for i := 0; i < sec; i++ {
		toks = append(toks, engine.Token{Text: fmt.Sprintf("▁w%d", i), Start: float32(i), End: float32(i) + 0.5})
	}
	return "", toks
}

func TestTranscribeMeetingWindowed(t *testing.T) {
	samples := make([]float32, 30*pipelineRate)
	timeline := []Turn{{Start: 2, End: 6, Label: 0}, {Start: 10, End: 14, Label: 1}}
	f := &fakeRecognizer{timestamps: true}
	words := TranscribeMeeting(f, samples, timeline)
	if len(f.calls) != 1 { // 2..14 fits one window
		t.Fatalf("calls = %v", f.calls)
	}
	if len(words) != 12 {
		t.Fatalf("words = %d, want 12 (one per second of 12s window)", len(words))
	}
	if words[0].Start != 2.0 || words[0].Text != " w0" {
		t.Errorf("first word %+v — offset not applied?", words[0])
	}
}

func TestTranscribeMeetingPerTurnFallback(t *testing.T) {
	samples := make([]float32, 30*pipelineRate)
	timeline := []Turn{{Start: 2, End: 6, Label: 0}, {Start: 10, End: 14, Label: 1}}
	f := &fakeRecognizer{timestamps: false}
	words := TranscribeMeeting(f, samples, timeline)
	want := []Word{
		{Text: " turn text here", Start: 2, End: 6},
		{Text: " turn text here", Start: 10, End: 14},
	}
	if !reflect.DeepEqual(words, want) {
		t.Errorf("got %+v\nwant %+v", words, want)
	}
}
