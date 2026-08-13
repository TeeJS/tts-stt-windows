// Package wyoming implements the Wyoming voice protocol (github.com/rhasspy/wyoming) wire
// framing, server-side. Each event on the wire is:
//
//	<header-json>\n
//	<data-json bytes>      exactly header.data_length bytes, present when data_length > 0
//	<binary payload bytes> exactly header.payload_length bytes, present when payload_length > 0
//
// Real Wyoming servers externalize reply data into the data_length block; inline header.data is
// a convenience form some clients send. This reader accepts both (data_length wins); the writer
// always emits the data_length form, matching wyoming-faster-whisper/wyoming-piper behavior.
// Reference implementations this was written against: lucidtype/transcriber.go (Go client) and
// open-quake app/claudevoice-wyoming.js (JS client, verified against the real Python servers).
package wyoming

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

// Generous ceilings so a malformed or malicious header can't make us allocate unbounded memory.
// Utterances are seconds of PCM and synthesized replies are sentence-sized; 100MB is far beyond
// anything legitimate.
const (
	maxDataLen    = 10 * 1024 * 1024
	maxPayloadLen = 100 * 1024 * 1024
)

// Event is one decoded protocol event.
type Event struct {
	Type    string
	Data    map[string]any // nil when the event carries no data
	Payload []byte         // nil when the event carries no payload
}

type header struct {
	Type          string          `json:"type"`
	Data          json.RawMessage `json:"data,omitempty"`
	DataLength    int             `json:"data_length,omitempty"`
	PayloadLength int             `json:"payload_length,omitempty"`
}

// ReadEvent reads one complete event, blocking until it arrives or the stream errors/closes.
func ReadEvent(r *bufio.Reader) (*Event, error) {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var h header
	if err := json.Unmarshal(line, &h); err != nil {
		return nil, fmt.Errorf("wyoming: bad header %q: %w", truncate(line, 200), err)
	}
	if h.DataLength < 0 || h.DataLength > maxDataLen {
		return nil, fmt.Errorf("wyoming: unreasonable data_length %d", h.DataLength)
	}
	if h.PayloadLength < 0 || h.PayloadLength > maxPayloadLen {
		return nil, fmt.Errorf("wyoming: unreasonable payload_length %d", h.PayloadLength)
	}
	ev := &Event{Type: h.Type}
	raw := h.Data // inline form (client convenience)
	if h.DataLength > 0 {
		block := make([]byte, h.DataLength)
		if _, err := io.ReadFull(r, block); err != nil {
			return nil, fmt.Errorf("wyoming: short data block: %w", err)
		}
		raw = block // data_length block wins over inline data
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &ev.Data); err != nil {
			return nil, fmt.Errorf("wyoming: bad data block for %q: %w", h.Type, err)
		}
	}
	if h.PayloadLength > 0 {
		ev.Payload = make([]byte, h.PayloadLength)
		if _, err := io.ReadFull(r, ev.Payload); err != nil {
			return nil, fmt.Errorf("wyoming: short payload: %w", err)
		}
	}
	return ev, nil
}

// WriteEvent writes one event in the externalized-data form real servers use.
// data may be nil (no data block); any JSON-marshalable value is accepted.
func WriteEvent(w io.Writer, typ string, data any, payload []byte) error {
	var dataBytes []byte
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return fmt.Errorf("wyoming: marshal data for %q: %w", typ, err)
		}
		dataBytes = b
	}
	h := header{Type: typ, DataLength: len(dataBytes), PayloadLength: len(payload)}
	hb, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if _, err := w.Write(append(hb, '\n')); err != nil {
		return err
	}
	if len(dataBytes) > 0 {
		if _, err := w.Write(dataBytes); err != nil {
			return err
		}
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
