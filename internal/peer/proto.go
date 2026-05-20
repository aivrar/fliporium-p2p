// Wire protocol for the Fliporium peer link.
//
// Frames are length-prefixed JSON: 4-byte big-endian length, then a JSON
// Envelope object. The Envelope wraps a typed body so the reader can
// dispatch by Type without unmarshalling the body twice.
package peer

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

const ProtocolVersion = "fliporium/0.5"

type MessageType string

const (
	TypeHello   MessageType = "HELLO"
	TypeMessage MessageType = "MESSAGE"
	TypeBye     MessageType = "BYE"

	// Flip = file transfer. v0.5 carries chunks as base64 inside JSON envelopes;
	// future versions will switch to native binary framing for efficiency.
	TypeFlipStart  MessageType = "FLIP_START"
	TypeFlipChunk  MessageType = "FLIP_CHUNK"
	TypeFlipEnd    MessageType = "FLIP_END"
	TypeFlipAck    MessageType = "FLIP_ACK"
	TypeFlipReject MessageType = "FLIP_REJECT"
)

type Envelope struct {
	Type MessageType     `json:"type"`
	Body json.RawMessage `json:"body,omitempty"`
}

type Hello struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type Message struct {
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

type Bye struct {
	Reason string `json:"reason,omitempty"`
}

// FlipStart announces that the sender is about to begin streaming a file.
type FlipStart struct {
	ID       string `json:"id"`       // sender-chosen UUID
	Filename string `json:"filename"` // basename only; receiver decides path
	Size     int64  `json:"size"`     // bytes
	Mime     string `json:"mime,omitempty"`
}

// FlipChunk carries up to ChunkSize bytes of file data.
type FlipChunk struct {
	ID     string `json:"id"`
	Offset int64  `json:"offset"`
	Data   []byte `json:"data"` // base64-encoded by encoding/json
}

// FlipEnd terminates a flip; the receiver verifies Sha256.
type FlipEnd struct {
	ID     string `json:"id"`
	Sha256 string `json:"sha256"` // hex-encoded
}

// FlipAck reports the cumulative number of bytes the receiver has on disk.
type FlipAck struct {
	ID       string `json:"id"`
	Received int64  `json:"received"`
}

// FlipReject is the receiver telling the sender to stop (e.g. user declined).
type FlipReject struct {
	ID     string `json:"id"`
	Reason string `json:"reason,omitempty"`
}

// ChunkSize is the size of each FlipChunk payload in bytes.
// 64KB strikes a balance between framing overhead and memory pressure.
const ChunkSize = 64 * 1024

// MaxFrame caps a single envelope to ~256KB after base64 expansion, leaving
// headroom around ChunkSize.
const MaxFrame = 256 * 1024

func WriteFrame(w io.Writer, t MessageType, body any) error {
	var bodyJSON json.RawMessage
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal body: %w", err)
		}
		bodyJSON = b
	}
	payload, err := json.Marshal(Envelope{Type: t, Body: bodyJSON})
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	if uint32(len(payload)) > MaxFrame {
		return fmt.Errorf("frame too large: %d > %d", len(payload), MaxFrame)
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}
	_, err = w.Write(payload)
	return err
}

func ReadFrame(r io.Reader) (Envelope, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return Envelope{}, err
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	if n > MaxFrame {
		return Envelope{}, fmt.Errorf("frame too large: %d > %d", n, MaxFrame)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return Envelope{}, fmt.Errorf("read payload: %w", err)
	}
	var env Envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return Envelope{}, fmt.Errorf("unmarshal envelope: %w", err)
	}
	return env, nil
}
