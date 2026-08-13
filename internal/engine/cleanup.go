package engine

import (
	"regexp"
	"strings"
)

// Speech models describe sounds they can't transcribe rather than staying silent: Whisper writes
// "(clicking)", "[BLANK_AUDIO]" or "♪♪♪" for keyboard noise, fans and music, and hallucinates
// stock phrases like "Thank you for watching" on near-silence. Downstream those arrive as if the
// user had spoken them — a click of the keyboard becomes a turn sent to the agent.
//
// CleanTranscript drops a transcript that is nothing but annotation, and strips annotations that
// merely bracket real speech. It never rewrites the words themselves.

var (
	// (clicking), [BLANK_AUDIO], *sighs*, ♪ music ♪ — any bracketed or asterisked aside.
	annotationRe = regexp.MustCompile(`[\(\[\*][^\)\]\*]{0,60}[\)\]\*]`)
	// Musical notes mark music or singing: "♪♪♪" alone, or "♪ lyrics or a description ♪". Either
	// way the model heard something that isn't the user talking to it, so the marked span goes
	// entirely rather than being passed along as if it had been said.
	musicPhraseRe = regexp.MustCompile(`[♪♫🎵🎶][^♪♫🎵🎶]{0,80}[♪♫🎵🎶]`)
	musicRe       = regexp.MustCompile(`[♪♫🎵🎶]+`)
	// Left over once annotations are gone: punctuation, whitespace, nothing worth sending.
	onlyPunctRe = regexp.MustCompile(`^[\s\p{P}\p{S}]*$`)
)

// Stock phrases models hallucinate on silence or noise. Matched only when they are the ENTIRE
// transcript, case- and punctuation-insensitively — someone genuinely saying "thanks for
// watching" mid-sentence still gets through.
var hallucinations = []string{
	"thank you",
	"thanks for watching",
	"thank you for watching",
	"thanks for watching!",
	"you",
	"bye",
	"bye bye",
	"okay",
	"so",
	"subtitles by the amara org community",
	"subscribe",
	"please subscribe",
	"transcription by castingwords",
	"amen",
}

// CleanTranscript returns the transcript with non-speech annotations removed, or "" if nothing
// but noise was recognized. An empty result means "heard nothing" to every caller.
func CleanTranscript(text string) string {
	s := strings.TrimSpace(text)
	if s == "" {
		return ""
	}
	s = musicPhraseRe.ReplaceAllString(s, " ")
	s = musicRe.ReplaceAllString(s, " ")
	s = annotationRe.ReplaceAllString(s, " ")
	s = strings.Join(strings.Fields(s), " ") // collapse the gaps the removals left behind
	if s == "" || onlyPunctRe.MatchString(s) {
		return ""
	}
	if isHallucination(s) {
		return ""
	}
	return s
}

// isHallucination reports whether the whole transcript is one of the known stock phrases.
func isHallucination(s string) bool {
	norm := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == ' ':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		case r == '\'' || r == '’':
			return -1 // drop apostrophes so "don't"/"dont" compare equal
		}
		return ' '
	}, s)
	norm = strings.Join(strings.Fields(norm), " ")
	for _, h := range hallucinations {
		if norm == h {
			return true
		}
	}
	return false
}
