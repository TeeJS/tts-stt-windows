//go:build windows

package engine

import "testing"

// Exercises the real Windows SAPI stack — no model files, no network. If Windows ever stops
// providing SAPI.SpVoice this fails loudly rather than silently degrading the fallback voice.
func TestSapiSynthesize(t *testing.T) {
	s := NewSapiTTS()
	defer s.Close()
	pcm, rate, err := s.Synthesize("Testing the built in Windows voice.")
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if rate < 8000 || rate > 48000 {
		t.Fatalf("implausible sample rate %d", rate)
	}
	secs := float64(len(pcm)) / 2 / float64(rate)
	if secs < 1 {
		t.Fatalf("audio too short: %.2fs (%d bytes @ %dHz)", secs, len(pcm), rate)
	}
	var loud int
	for i := 0; i+1 < len(pcm); i += 2 {
		v := int16(uint16(pcm[i]) | uint16(pcm[i+1])<<8)
		if v > 1638 || v < -1638 { // >5% of full scale
			loud++
		}
	}
	if loud < 1000 {
		t.Fatalf("audio looks like silence: only %d loud samples in %d", loud, len(pcm)/2)
	}
	t.Logf("SAPI produced %.2fs at %dHz, %d%% loud samples", secs, rate, 100*loud/(len(pcm)/2))
}
