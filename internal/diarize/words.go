package diarize

import (
	"strings"

	"github.com/TeeJS/tts-stt-windows/internal/engine"
)

// Transcriber is the slice of engine.Recognizer the meeting pipeline needs.
type Transcriber interface {
	TranscribeTokens(samples []float32, sampleRate int) (string, []engine.Token)
}

// maxWindowSec bounds how much audio goes through the recognizer at once. Windows
// are cut at diarization gaps so words never straddle a boundary; transducer models
// handle ~1 minute comfortably where whisper-style 30s framing would truncate.
const maxWindowSec = 50.0

// Word is one transcribed word with its timing. Text carries a leading space
// (the Whisper/SentencePiece convention) so segment text is built by plain
// concatenation, matching the Python pipeline.
type Word struct {
	Text  string
	Start float64
	End   float64
}

const unknownSpeaker = -1

// speechWindows plans transcription windows over the diarized timeline: each window
// spans consecutive turns, closing at a gap once adding the next turn would push it
// past maxWindowSec. A single turn longer than the cap is split mid-turn.
func speechWindows(timeline []Turn, totalSec float64) [][2]float64 {
	if len(timeline) == 0 {
		return nil
	}
	var windows [][2]float64
	start := timeline[0].Start
	end := timeline[0].End
	for _, t := range timeline[1:] {
		if t.End-start > maxWindowSec && t.Start > end {
			windows = append(windows, [2]float64{start, end})
			start = t.Start
		}
		if t.End > end {
			end = t.End
		}
	}
	windows = append(windows, [2]float64{start, end})

	// Hard-split any window that still exceeds the cap (one very long turn, or
	// wall-to-wall speech with no usable gap).
	var out [][2]float64
	for _, w := range windows {
		for w[1]-w[0] > maxWindowSec {
			out = append(out, [2]float64{w[0], w[0] + maxWindowSec})
			w[0] += maxWindowSec
		}
		if w[1] > w[0] {
			if w[1] > totalSec {
				w[1] = totalSec
			}
			out = append(out, w)
		}
	}
	return out
}

// tokensToWords converts subword tokens to words. A word boundary is a token
// starting with "▁" (raw SentencePiece) or " " (models whose tokens.txt already
// renders ▁ as a space, e.g. parakeet). Word text keeps a leading space (the
// Whisper convention the Python pipeline's plain-concatenation segment building
// relies on). offset shifts token times from window-relative to recording-relative.
func tokensToWords(toks []engine.Token, offset float64) []Word {
	var words []Word
	for _, tk := range toks {
		text := tk.Text
		newWord := false
		if strings.HasPrefix(text, "▁") {
			newWord = true
			text = " " + strings.TrimPrefix(text, "▁")
		} else if strings.HasPrefix(text, " ") {
			newWord = true
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		if newWord || len(words) == 0 {
			if !strings.HasPrefix(text, " ") {
				text = " " + text
			}
			words = append(words, Word{
				Text:  text,
				Start: offset + float64(tk.Start),
				End:   offset + float64(tk.End),
			})
		} else {
			w := &words[len(words)-1]
			w.Text += text
			if e := offset + float64(tk.End); e > w.End {
				w.End = e
			}
		}
	}
	return words
}

// TranscribeMeeting produces word-timed transcription of a whole recording (16 kHz
// mono samples): diarization-gap-bounded windows through the recognizer, token
// timestamps offset back to recording time. If the model family produces no token
// timestamps, it falls back to transcribing each diarized turn separately, emitting
// the turn's text as one pseudo-word spanning the turn — coarser boundaries, but
// correct attribution with any model.
func TranscribeMeeting(t Transcriber, samples []float32, timeline []Turn) []Word {
	crop := func(startSec, endSec float64) []float32 {
		lo, hi := int(startSec*pipelineRate), int(endSec*pipelineRate)
		if lo < 0 {
			lo = 0
		}
		if hi > len(samples) {
			hi = len(samples)
		}
		if hi <= lo {
			return nil
		}
		return samples[lo:hi]
	}

	var words []Word
	perTurn := false
	for i, w := range speechWindows(timeline, float64(len(samples))/pipelineRate) {
		text, toks := t.TranscribeTokens(crop(w[0], w[1]), pipelineRate)
		if i == 0 && len(toks) == 0 && strings.TrimSpace(text) != "" {
			perTurn = true
			break
		}
		words = append(words, tokensToWords(toks, w[0])...)
	}
	if !perTurn {
		return words
	}

	words = nil
	for _, turn := range timeline {
		text, _ := t.TranscribeTokens(crop(turn.Start, turn.End), pipelineRate)
		if text = strings.TrimSpace(text); text != "" {
			words = append(words, Word{Text: " " + text, Start: turn.Start, End: turn.End})
		}
	}
	return words
}

// attributeWords ports the word→speaker logic from Diarizer.diarize plus
// _fill_unknown_words and _words_to_segments:
//
//  1. Each word goes to the first diarized turn containing its midpoint, else UNKNOWN.
//  2. Maximal runs of UNKNOWN words with the SAME speaker on both sides are filled
//     with that speaker — pyannote turn edges have slivers Whisper word timings
//     don't respect, and 96% of unattributed words on real meetings sit mid-sentence
//     inside one person's speech. Runs at a genuine speaker change stay UNKNOWN:
//     that is the honest answer, and it marks the handover.
//  3. Consecutive same-speaker words merge into segments; empty ones are dropped.
func attributeWords(words []Word, timeline []Turn, labelMap map[int]string) []Segment {
	if len(words) == 0 {
		return []Segment{}
	}

	speakers := make([]int, len(words))
	for i, w := range words {
		mid := (w.Start + w.End) / 2
		speakers[i] = unknownSpeaker
		for _, t := range timeline {
			if t.Start <= mid && mid <= t.End {
				speakers[i] = t.Label
				break
			}
		}
	}

	// Fill UNKNOWN runs flanked by the same speaker.
	for i := 0; i < len(words); {
		if speakers[i] != unknownSpeaker {
			i++
			continue
		}
		j := i
		for j < len(words) && speakers[j] == unknownSpeaker {
			j++
		}
		if i > 0 && j < len(words) && speakers[i-1] == speakers[j] {
			for k := i; k < j; k++ {
				speakers[k] = speakers[i-1]
			}
		}
		i = j
	}

	display := func(spk int) string {
		if spk == unknownSpeaker {
			return "UNKNOWN"
		}
		if name, ok := labelMap[spk]; ok {
			return name
		}
		return "UNKNOWN"
	}

	segments := make([]Segment, 0)
	cur := speakers[0]
	text := words[0].Text
	start, end := words[0].Start, words[0].End
	flush := func() {
		if t := strings.TrimSpace(text); t != "" {
			segments = append(segments, Segment{
				Speaker: display(cur), Start: round2(start), End: round2(end), Text: t,
			})
		}
	}
	for i := 1; i < len(words); i++ {
		if speakers[i] == cur {
			text += words[i].Text
			end = words[i].End
			continue
		}
		flush()
		cur = speakers[i]
		text = words[i].Text
		start, end = words[i].Start, words[i].End
	}
	flush()
	return segments
}
