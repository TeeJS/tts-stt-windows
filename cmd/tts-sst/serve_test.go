package main

import (
	"net"
	"testing"
	"time"

	"github.com/TeeJS/tts-stt-windows/internal/config"
	"github.com/TeeJS/tts-stt-windows/internal/models"
)

// resolve decides what a configured value means. "off" is a deliberate choice and must survive;
// an empty or unknown value must fall back so a stale config never leaves the app with nothing.
func TestResolve(t *testing.T) {
	s := newService(config.Defaults(), t.TempDir(), 2)
	cases := []struct {
		in, want string
		kind     models.Kind
		why      string
	}{
		{"", "fallback", models.TTS, "unset means pick a default"},
		{offID, offID, models.TTS, "off must not be overridden by a default"},
		{offID, offID, models.STT, "off must not be overridden by a default"},
		{sapiVoiceID, sapiVoiceID, models.TTS, "built-in voice is valid but not in the catalog"},
		{"no-such-model", "fallback", models.STT, "unknown id falls back"},
		{"piper-en-GB-alan-medium", "piper-en-GB-alan-medium", models.TTS, "catalog id passes through"},
		{"piper-en-GB-alan-medium", "fallback", models.STT, "right id, wrong service, falls back"},
	}
	for _, c := range cases {
		if got := s.resolve(c.in, c.kind, "fallback"); got != c.want {
			t.Errorf("%s: resolve(%q, %s) = %q, want %q", c.why, c.in, c.kind, got, c.want)
		}
	}
}

func free(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func listening(port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(port)), 500*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

// Turning a service off must free its port, not leave something bound that answers with nothing —
// and turning it back on must start listening again without a restart.
func TestListenerLifecycle(t *testing.T) {
	cfg := config.Defaults()
	cfg.STTPort, cfg.TTSPort = free(t), free(t)
	s := newService(cfg, t.TempDir(), 2)

	// Both on: engines are irrelevant here, only the listeners are under test.
	s.sttID, s.ttsID = "some-model", "some-voice"
	s.syncListeners()
	t.Cleanup(func() { s.sttID, s.ttsID = offID, offID; s.syncListeners() })

	if !listening(cfg.STTPort) || !listening(cfg.TTSPort) {
		t.Fatal("both ports should be open when both services are on")
	}

	// Switch STT off; its port closes, TTS is untouched.
	s.sttID = offID
	s.syncListeners()
	if listening(cfg.STTPort) {
		t.Error("STT port still bound after switching the service off")
	}
	if !listening(cfg.TTSPort) {
		t.Error("switching STT off must not disturb TTS")
	}

	// Switch it back on: listening again, no restart involved.
	s.sttID = "some-model"
	s.syncListeners()
	if !listening(cfg.STTPort) {
		t.Error("STT port should reopen when the service is switched back on")
	}

	// Repeated calls are a no-op rather than an error (the UI calls this on every switch).
	s.syncListeners()
	if !listening(cfg.STTPort) || !listening(cfg.TTSPort) {
		t.Error("syncListeners must be idempotent")
	}
}

// Discovery must reflect the CURRENT state, not the state when the listener happened to start.
// Regression test: switching TTS off left the still-running STT listener advertising a
// text-to-speech service whose port had just been closed, so a client that discovered services
// there would try to connect to nothing.
func TestInfoFollowsServiceState(t *testing.T) {
	s := newService(config.Defaults(), t.TempDir(), 2)
	s.sttID, s.ttsID = "parakeet-tdt-0.6b-v3", "piper-en-GB-alan-medium"
	if info := s.info(); info["asr"] == nil || info["tts"] == nil {
		t.Fatal("both services running: both should be advertised")
	}
	// Turn TTS off WITHOUT touching the STT listener, exactly as the UI does.
	s.ttsID = offID
	info := s.info()
	if info["tts"] != nil {
		t.Error("a service that is off must not be advertised")
	}
	if info["asr"] == nil {
		t.Error("the service still running must stay advertised")
	}
	s.sttID = offID
	if info := s.info(); len(info) != 0 {
		t.Errorf("both off should advertise nothing, got %v", info)
	}
}

// A service that is off must not be advertised to Wyoming discovery: a client shouldn't be told
// about something that isn't running.
func TestAdvertised(t *testing.T) {
	if got := advertised(offID); got != "" {
		t.Errorf("advertised(off) = %q, want empty so BuildInfo omits the service", got)
	}
	if got := advertised(""); got != "" {
		t.Errorf("advertised(unset) = %q, want empty", got)
	}
	if got := advertised(sapiVoiceID); got == "" {
		t.Error("a running service must be advertised")
	}
}
