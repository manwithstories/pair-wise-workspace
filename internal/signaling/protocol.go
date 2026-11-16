// Package signaling defines the wire protocol spoken between the relay
// command and its loopback signaling server.
//
// The protocol is deliberately tiny: newline-delimited JSON messages over a
// single TCP connection. Every message is a Message envelope carrying a type,
// a monotonically increasing sequence number and an optional session id. The
// envelope exists so that the server can reject malformed input, duplicate
// deliveries and messages addressed to unknown sessions with a precise error
// instead of silently dropping them.
package signaling

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Kind enumerates every message type the protocol understands.
type Kind string

const (
	// KindOffer carries the SDP offer from the offering peer.
	KindOffer Kind = "offer"
	// KindAnswer carries the SDP answer from the answering peer.
	KindAnswer Kind = "answer"
	// KindCandidate carries a single trickled ICE candidate.
	KindCandidate Kind = "candidate"
	// KindState reports a transport state transition for a session.
	KindState Kind = "state"
	// KindError reports a server-side failure for a session.
	KindError Kind = "error"
)

// Valid reports whether k is one of the protocol message types.
func (k Kind) Valid() bool {
	switch k {
	case KindOffer, KindAnswer, KindCandidate, KindState, KindError:
		return true
	default:
		return false
	}
}

// Message is the single envelope exchanged over the signaling socket.
//
// Sequence numbers are per-sender and strictly increasing. The server uses
// them to make duplicate deliveries observable instead of silently appending
// the same offer or candidate twice.
type Message struct {
	Type Kind `json:"type"`
	// Seq is the per-sender message sequence number. A repeated Seq is a
	// duplicate delivery and is rejected.
	Seq uint64 `json:"seq"`
	// Session identifies the signaling conversation. It is empty on the
	// first message (the offer) and required afterwards.
	Session string `json:"session,omitempty"`
	// SDP carries an offer or answer payload.
	SDP string `json:"sdp,omitempty"`
	// Candidate carries one trickled ICE candidate in SDP attribute form.
	Candidate string `json:"candidate,omitempty"`
	// State carries a transport state label for KindState.
	State string `json:"state,omitempty"`
	// Code is a stable machine-readable identifier on KindError.
	Code string `json:"code,omitempty"`
	// Detail is a human-readable explanation on KindError.
	Detail string `json:"detail,omitempty"`
}

// Error codes shared by the server and the CLI. They are stable strings so
// tests and scripts can match on them.
const (
	ErrCodeInvalidJSON     = "invalid_json"
	ErrCodeUnknownType     = "unknown_type"
	ErrCodeDuplicate       = "duplicate_message"
	ErrCodeOutOfOrder      = "out_of_order_message"
	ErrCodeUnknownSession  = "unknown_session"
	ErrCodeMissingSession  = "missing_session"
	ErrCodeSessionMismatch = "session_mismatch"
	ErrCodeEmptyPayload    = "empty_payload"
	ErrCodeTimeout         = "signaling_timeout"
	ErrCodeClosed          = "signaling_closed"
	ErrCodeNonLoopback     = "non_loopback_candidate"
	ErrCodeInternal        = "internal_error"
)

// ProtocolError is returned for any message that violates the protocol. It is
// always a value error, never a panic, so a hostile or buggy client cannot
// take the server down.
type ProtocolError struct {
	Code   string
	Detail string
}

func (e *ProtocolError) Error() string {
	if e.Detail == "" {
		return e.Code
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

// newProtocolError builds a *ProtocolError.
func newProtocolError(code, format string, args ...any) *ProtocolError {
	return &ProtocolError{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// ErrCode extracts the stable code from err if it is a ProtocolError.
func ErrCode(err error) string {
	var pe *ProtocolError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// Encode serializes m into a single newline-terminated JSON line.
func Encode(m Message) ([]byte, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encode message: %w", err)
	}
	return append(raw, '\n'), nil
}

// Decode parses one JSON line into a Message and validates the envelope.
func Decode(raw []byte) (Message, error) {
	var m Message
	if len(raw) == 0 {
		return m, newProtocolError(ErrCodeInvalidJSON, "empty message")
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, newProtocolError(ErrCodeInvalidJSON, "%s", err)
	}
	if err := m.Validate(); err != nil {
		return Message{}, err
	}
	return m, nil
}

// Validate checks the structural invariants of a decoded message.
func (m Message) Validate() error {
	if !m.Type.Valid() {
		return newProtocolError(ErrCodeUnknownType, "unknown message type %q", m.Type)
	}
	switch m.Type {
	case KindOffer:
		if m.SDP == "" {
			return newProtocolError(ErrCodeEmptyPayload, "offer without sdp")
		}
	case KindAnswer:
		if m.SDP == "" {
			return newProtocolError(ErrCodeEmptyPayload, "answer without sdp")
		}
		if m.Session == "" {
			return newProtocolError(ErrCodeMissingSession, "answer without session")
		}
	case KindCandidate:
		if m.Candidate == "" {
			return newProtocolError(ErrCodeEmptyPayload, "candidate without value")
		}
		if m.Session == "" {
			return newProtocolError(ErrCodeMissingSession, "candidate without session")
		}
	}
	return nil
}

// ErrorMessage builds a KindError reply for the given protocol code.
func ErrorMessage(seq uint64, session, code, detail string) Message {
	return Message{Type: KindError, Seq: seq, Session: session, Code: code, Detail: detail}
}
