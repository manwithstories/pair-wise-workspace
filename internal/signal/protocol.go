// Package signal implements a line-delimited JSON signaling protocol over
// TCP. Each message is a single JSON object terminated by a newline.
package signal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

// Kind enumerates signaling message types.
type Kind string

const (
	KindHello     Kind = "hello"
	KindOffer     Kind = "offer"
	KindAnswer    Kind = "answer"
	KindCandidate Kind = "candidate"
	KindBye       Kind = "bye"
	KindError     Kind = "error"
)

// MaxLine bounds the size of a single encoded message.
const MaxLine = 1 << 20

// Message is the unit exchanged between signaling peers.
type Message struct {
	Seq       uint64 `json:"seq"`
	Session   string `json:"session"`
	Kind      Kind   `json:"kind"`
	From      string `json:"from"`
	To        string `json:"to,omitempty"`
	Role      string `json:"role,omitempty"`
	SDP       string `json:"sdp,omitempty"`
	Candidate string `json:"candidate,omitempty"`
	Err       string `json:"error,omitempty"`
}

// Sequenced reports whether the kind participates in sequence-number based
// replay and staleness rejection.
func (k Kind) Sequenced() bool {
	switch k {
	case KindOffer, KindAnswer, KindCandidate:
		return true
	}
	return false
}

// Encode writes m to w as one JSON line.
func Encode(w io.Writer, m Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(b)+1 > MaxLine {
		return fmt.Errorf("signal: message too large (%d bytes)", len(b))
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// Decode reads one JSON line from r.
func Decode(r *bufio.Reader) (Message, error) {
	var m Message
	line, err := r.ReadBytes('\n')
	if err != nil {
		return m, err
	}
	if len(line) > MaxLine {
		return m, fmt.Errorf("signal: message exceeds %d bytes", MaxLine)
	}
	if err := json.Unmarshal(line, &m); err != nil {
		return m, fmt.Errorf("signal: decode: %w", err)
	}
	return m, nil
}
