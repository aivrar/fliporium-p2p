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

const ProtocolVersion = "fliporium/0.4"

type MessageType string

const (
	TypeHello   MessageType = "HELLO"
	TypeMessage MessageType = "MESSAGE"
	TypeBye     MessageType = "BYE"
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

const MaxFrame = 1 << 20

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
