package wyoming

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"testing"
	"time"
)

// The open-quake JS client (and lucidtype's Go client) write events with INLINE header.data —
// only servers use the externalized data_length form. The test client below therefore writes the
// inline form, so the test proves the same asymmetry the real wire has.
func writeInline(t *testing.T, conn net.Conn, typ string, dataJSON string, payload []byte) {
	t.Helper()
	h := fmt.Sprintf(`{"type":%q`, typ)
	if dataJSON != "" {
		h += `,"data":` + dataJSON
	}
	if len(payload) > 0 {
		h += fmt.Sprintf(`,"payload_length":%d`, len(payload))
	}
	h += "}\n"
	if _, err := conn.Write([]byte(h)); err != nil {
		t.Fatal(err)
	}
	if len(payload) > 0 {
		if _, err := conn.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func dial(t *testing.T, l net.Listener) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", l.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	return conn, bufio.NewReader(conn)
}

func TestSTTSession(t *testing.T) {
	l := listen(t)
	var gotPCM []byte
	var gotFormat AudioFormat
	var gotLang string
	go ServeSTT(l, func(pcm []byte, f AudioFormat, lang string) (string, error) {
		gotPCM, gotFormat, gotLang = append([]byte(nil), pcm...), f, lang
		return "hello world", nil
	}, BuildInfo("test-model", ""), t.Logf)

	conn, r := dial(t, l)
	pcm := bytes.Repeat([]byte{0x01, 0x02}, 4000)
	writeInline(t, conn, "transcribe", `{"language":"en"}`, nil)
	writeInline(t, conn, "audio-start", `{"rate":16000,"width":2,"channels":1}`, nil)
	writeInline(t, conn, "audio-chunk", `{"rate":16000,"width":2,"channels":1}`, pcm[:5000])
	writeInline(t, conn, "audio-chunk", `{"rate":16000,"width":2,"channels":1}`, pcm[5000:])
	writeInline(t, conn, "audio-stop", "", nil)

	ev, err := ReadEvent(r)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != "transcript" || ev.Data["text"] != "hello world" {
		t.Fatalf("got %q %v", ev.Type, ev.Data)
	}
	if !bytes.Equal(gotPCM, pcm) {
		t.Fatalf("PCM mangled: got %d bytes want %d", len(gotPCM), len(pcm))
	}
	if gotFormat != (AudioFormat{16000, 2, 1}) || gotLang != "en" {
		t.Fatalf("format/lang: %+v %q", gotFormat, gotLang)
	}
}

func TestTTSSession(t *testing.T) {
	l := listen(t)
	// 1.5 chunks of deterministic PCM so the chunking + reassembly path is exercised.
	pcm := make([]byte, chunkSize+chunkSize/2)
	for i := range pcm {
		pcm[i] = byte(i % 251)
	}
	go ServeTTS(l, func(text string) ([]byte, AudioFormat, error) {
		if text != "say this" {
			return nil, AudioFormat{}, fmt.Errorf("wrong text %q", text)
		}
		return pcm, AudioFormat{22050, 2, 1}, nil
	}, BuildInfo("", "test-voice"), t.Logf)

	conn, r := dial(t, l)
	writeInline(t, conn, "synthesize", `{"text":"say this"}`, nil)

	ev, err := ReadEvent(r)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != "audio-start" || ev.Data["rate"] != float64(22050) {
		t.Fatalf("got %q %v", ev.Type, ev.Data)
	}
	var got []byte
	for {
		ev, err = ReadEvent(r)
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == "audio-stop" {
			break
		}
		if ev.Type != "audio-chunk" {
			t.Fatalf("unexpected %q", ev.Type)
		}
		got = append(got, ev.Payload...)
	}
	if !bytes.Equal(got, pcm) {
		t.Fatalf("audio mangled: got %d bytes want %d", len(got), len(pcm))
	}
}

func TestDescribe(t *testing.T) {
	l := listen(t)
	go ServeTTS(l, func(string) ([]byte, AudioFormat, error) { return nil, AudioFormat{}, nil },
		BuildInfo("", "test-voice"), t.Logf)
	conn, r := dial(t, l)
	writeInline(t, conn, "describe", "", nil)
	ev, err := ReadEvent(r)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != "info" || ev.Data["tts"] == nil {
		t.Fatalf("got %q %v", ev.Type, ev.Data)
	}
}

// Round-trip through our own writer/reader (server form with data_length) — and confirm the
// header of a written event really externalizes data instead of inlining it.
func TestWriteEventExternalizesData(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteEvent(&buf, "audio-start", map[string]any{"rate": 22050}, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	line, _ := bytes.CutSuffix(buf.Bytes(), nil)
	nl := bytes.IndexByte(line, '\n')
	if !bytes.Contains(line[:nl], []byte(`"data_length"`)) || bytes.Contains(line[:nl], []byte(`"data"`)) && bytes.Contains(line[:nl], []byte(`"data":{`)) {
		t.Fatalf("header should externalize data: %s", line[:nl])
	}
	ev, err := ReadEvent(bufio.NewReader(&buf))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != "audio-start" || ev.Data["rate"] != float64(22050) || len(ev.Payload) != 3 {
		t.Fatalf("round trip: %+v", ev)
	}
	_ = binary.LittleEndian // keep imports honest if the assertion set shrinks
}
