package wyoming

import (
	"bufio"
	"errors"
	"io"
	"net"
)

// AudioFormat describes raw PCM: sample rate in Hz, sample width in bytes, channel count.
type AudioFormat struct {
	Rate     int `json:"rate"`
	Width    int `json:"width"`
	Channels int `json:"channels"`
}

// STTFunc transcribes one complete utterance of raw PCM in the given format.
type STTFunc func(pcm []byte, format AudioFormat, language string) (string, error)

// TTSFunc synthesizes text to raw PCM, reporting the PCM's actual format.
type TTSFunc func(text string) (pcm []byte, format AudioFormat, err error)

// Info is the payload served for a `describe` event — how Wyoming clients (Home Assistant in
// particular) discover what a server offers. Kept as loose maps: the schema is large, we only
// populate the fields discovery actually reads.
type Info map[string]any

// audio-chunk payload size on the wire. Matches observed wyoming-piper behavior; small enough
// that clients can start playback while later chunks are still being written.
const chunkSize = 2048

// ServeSTT accepts connections and runs the ASR session protocol on each:
// transcribe{language}? -> audio-start{fmt} -> audio-chunk(s)+PCM -> audio-stop => transcript{text}.
// Multiple sequential requests per connection are supported; connections may also be one-shot.
func ServeSTT(l net.Listener, stt STTFunc, info Info, logf func(string, ...any)) {
	serve(l, logf, func(conn net.Conn, r *bufio.Reader) error {
		var (
			language string
			format   AudioFormat
			pcm      []byte
		)
		for {
			ev, err := ReadEvent(r)
			if err != nil {
				return err
			}
			switch ev.Type {
			case "describe":
				if err := WriteEvent(conn, "info", info, nil); err != nil {
					return err
				}
			case "transcribe":
				language, _ = ev.Data["language"].(string)
			case "audio-start":
				format = formatFromData(ev.Data)
				pcm = pcm[:0]
			case "audio-chunk":
				pcm = append(pcm, ev.Payload...)
			case "audio-stop":
				text, err := stt(pcm, format, language)
				if err != nil {
					logf("stt error: %v", err)
					text = "" // an empty transcript is the protocol-level "heard nothing"
				}
				logf("stt: %d bytes -> %q", len(pcm), text)
				if err := WriteEvent(conn, "transcript", map[string]any{"text": text}, nil); err != nil {
					return err
				}
				pcm = pcm[:0]
			default:
				logf("stt: ignoring event %q", ev.Type)
			}
		}
	})
}

// ServeTTS accepts connections and runs the TTS session protocol on each:
// synthesize{text} => audio-start{fmt} -> audio-chunk(s)+PCM -> audio-stop.
func ServeTTS(l net.Listener, tts TTSFunc, info Info, logf func(string, ...any)) {
	serve(l, logf, func(conn net.Conn, r *bufio.Reader) error {
		for {
			ev, err := ReadEvent(r)
			if err != nil {
				return err
			}
			switch ev.Type {
			case "describe":
				if err := WriteEvent(conn, "info", info, nil); err != nil {
					return err
				}
			case "synthesize":
				text, _ := ev.Data["text"].(string)
				if text == "" {
					logf("tts: synthesize with empty text, ignoring")
					continue
				}
				pcm, format, err := tts(text)
				if err != nil {
					logf("tts error: %v", err)
					return err // client sees the connection close, its own timeout/error path handles it
				}
				logf("tts: %q -> %d bytes @ %dHz", truncate([]byte(text), 60), len(pcm), format.Rate)
				if err := streamAudio(conn, pcm, format); err != nil {
					return err
				}
			default:
				logf("tts: ignoring event %q", ev.Type)
			}
		}
	})
}

// streamAudio writes audio-start, the PCM as chunkSize audio-chunks, then audio-stop.
func streamAudio(w io.Writer, pcm []byte, format AudioFormat) error {
	data := map[string]any{"rate": format.Rate, "width": format.Width, "channels": format.Channels, "timestamp": nil}
	if err := WriteEvent(w, "audio-start", data, nil); err != nil {
		return err
	}
	for off := 0; off < len(pcm); off += chunkSize {
		end := off + chunkSize
		if end > len(pcm) {
			end = len(pcm)
		}
		if err := WriteEvent(w, "audio-chunk", data, pcm[off:end]); err != nil {
			return err
		}
	}
	return WriteEvent(w, "audio-stop", map[string]any{"timestamp": nil}, nil)
}

// serve runs the accept loop, one goroutine per connection. A connection handler returning
// io.EOF (client hung up) is the normal end of a session, not an error.
func serve(l net.Listener, logf func(string, ...any), session func(net.Conn, *bufio.Reader) error) {
	for {
		conn, err := l.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				logf("accept error: %v", err)
			}
			return
		}
		go func() {
			defer conn.Close()
			err := session(conn, bufio.NewReader(conn))
			if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				logf("session ended with error: %v", err)
			}
		}()
	}
}

func formatFromData(d map[string]any) AudioFormat {
	f := AudioFormat{Rate: 16000, Width: 2, Channels: 1} // the Wyoming/Whisper convention
	if d == nil {
		return f
	}
	if v, ok := d["rate"].(float64); ok && v > 0 {
		f.Rate = int(v)
	}
	if v, ok := d["width"].(float64); ok && v > 0 {
		f.Width = int(v)
	}
	if v, ok := d["channels"].(float64); ok && v > 0 {
		f.Channels = int(v)
	}
	return f
}

// BuildInfo assembles a minimal `info` reply advertising one ASR and/or one TTS service —
// enough for Home Assistant's discovery to list the server and its model/voice.
func BuildInfo(asrModel, ttsVoice string) Info {
	svc := func(name, desc string, attr string, entries []map[string]any) map[string]any {
		return map[string]any{
			"name": name, "description": desc, "attribution": map[string]any{"name": "tts-sst", "url": "https://github.com/TeeJS/tts-stt-windows"},
			"installed": true, "version": "0.1.0", attr: entries,
		}
	}
	info := Info{}
	if asrModel != "" {
		info["asr"] = []map[string]any{svc("tts-sst", "Local Windows speech-to-text", "models", []map[string]any{
			{"name": asrModel, "description": asrModel, "installed": true, "languages": []string{"en"},
				"attribution": map[string]any{"name": "sherpa-onnx", "url": "https://github.com/k2-fsa/sherpa-onnx"}, "version": "1"},
		})}
	}
	if ttsVoice != "" {
		info["tts"] = []map[string]any{svc("tts-sst", "Local Windows text-to-speech", "voices", []map[string]any{
			{"name": ttsVoice, "description": ttsVoice, "installed": true, "languages": []string{"en"},
				"attribution": map[string]any{"name": "sherpa-onnx", "url": "https://github.com/k2-fsa/sherpa-onnx"}, "version": "1"},
		})}
	}
	return info
}
