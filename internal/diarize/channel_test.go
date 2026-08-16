package diarize

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestDecodeAudioChannelsStereo(t *testing.T) {
	// 16 kHz stereo: left ramps up, right is silent — verify they stay separate.
	var payload bytes.Buffer
	for i := 0; i < 100; i++ {
		binary.Write(&payload, binary.LittleEndian, int16(i*100)) // left
		binary.Write(&payload, binary.LittleEndian, int16(0))     // right
	}
	wav := buildWAV(1, 2, 16000, 16, payload.Bytes(), false)
	mono, chans, err := DecodeAudioChannels(wav)
	if err != nil {
		t.Fatal(err)
	}
	if len(chans) != 2 {
		t.Fatalf("channels = %d, want 2", len(chans))
	}
	if len(mono) != 100 || len(chans[0]) != 100 || len(chans[1]) != 100 {
		t.Fatalf("lengths mono=%d L=%d R=%d", len(mono), len(chans[0]), len(chans[1]))
	}
	// Right is silent; left is not.
	var lEnergy, rEnergy float64
	for i := range chans[0] {
		lEnergy += float64(chans[0][i]) * float64(chans[0][i])
		rEnergy += float64(chans[1][i]) * float64(chans[1][i])
	}
	if lEnergy == 0 || rEnergy != 0 {
		t.Errorf("expected left energy > 0 and right == 0, got L=%v R=%v", lEnergy, rEnergy)
	}
	// Mono is the average, so ~ half of left.
	if mono[50] == 0 || mono[50] >= chans[0][50] {
		t.Errorf("mono downmix wrong: mono[50]=%v left[50]=%v", mono[50], chans[0][50])
	}
}

func TestDecodeAudioChannelsMono(t *testing.T) {
	wav := buildWAV(1, 1, 16000, 16, s16le(0, 16384, -16384), false)
	mono, chans, err := DecodeAudioChannels(wav)
	if err != nil {
		t.Fatal(err)
	}
	if len(chans) != 1 {
		t.Fatalf("channels = %d, want 1 for mono", len(chans))
	}
	if len(mono) != 3 {
		t.Fatalf("mono len = %d", len(mono))
	}
}

// makeSignal returns a 16 kHz signal with unit energy during [start,end)s and
// silence elsewhere, up to durSec.
func makeSignal(durSec float64, spans ...[2]float64) []float32 {
	sig := make([]float32, int(durSec*pipelineRate))
	for _, sp := range spans {
		lo, hi := int(sp[0]*pipelineRate), int(sp[1]*pipelineRate)
		for i := lo; i < hi && i < len(sig); i++ {
			sig[i] = 0.5
		}
	}
	return sig
}

func TestMeHintIsMe(t *testing.T) {
	// Left (me) loud during 0-2s; Right (others) loud during 3-5s.
	left := makeSignal(6, [2]float64{0, 2})
	right := makeSignal(6, [2]float64{3, 5})
	me := &MeHint{Name: "T.J. Schmitz", Channel: 0, Signals: [][]float32{left, right}}

	meSegs := []Turn{{Start: 0, End: 2, Label: 0}}
	otherSegs := []Turn{{Start: 3, End: 5, Label: 1}}
	if !me.isMe(meSegs) {
		t.Error("cluster on the mic channel should be identified as me")
	}
	if me.isMe(otherSegs) {
		t.Error("cluster on the other channel must NOT be identified as me")
	}

	// A mono file (one channel) disables the hint entirely.
	monoHint := &MeHint{Name: "X", Channel: 0, Signals: [][]float32{left}}
	if monoHint.active() || monoHint.isMe(meSegs) {
		t.Error("hint must be inert with fewer than two channels")
	}
	// Nil hint is inert.
	var nilHint *MeHint
	if nilHint.active() || nilHint.isMe(meSegs) {
		t.Error("nil hint must be inert")
	}
}

func TestMeHintBleedRejected(t *testing.T) {
	// Both channels have energy during the segment, but the other channel dominates
	// (this cluster is someone else; the mic only caught faint bleed). Must not be me.
	left := make([]float32, 2*pipelineRate)
	right := make([]float32, 2*pipelineRate)
	for i := range left {
		left[i] = 0.05 // faint bleed on the mic
		right[i] = 0.5 // the actual speaker on loopback
	}
	me := &MeHint{Name: "Me", Channel: 0, Signals: [][]float32{left, right}}
	if me.isMe([]Turn{{Start: 0, End: 2, Label: 0}}) {
		t.Error("a bleed-only mic channel must not be claimed as me")
	}
}
