// Package signal implements the newline-delimited JSON signalling protocol
// used by relayctl. It deliberately has no dependency on Pion: only SDP blobs
// and ICE candidates cross this boundary.
package signal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"

	"github.com/pion/ice/v4"
)

// MessageType is the discriminator of a wire message.
type MessageType string

const (
	TypeHello     MessageType = "hello"
	TypeOffer     MessageType = "offer"
	TypeAnswer    MessageType = "answer"
	TypeCandidate MessageType = "candidate"
	TypeBye       MessageType = "bye"
	TypeHeartbeat MessageType = "heartbeat"
	// Server-originated control messages.
	TypeReady MessageType = "ready"
	TypeError MessageType = "error"
)

// Role identifies which side of a two-party session sent a message.
type Role string

const (
	RoleSender Role = "sender"
	RoleRelay  Role = "relay"
)

// ErrorCode is stable and intended for automation and CLI exit mapping.
type ErrorCode string

const (
	CodeInvalidMessage     ErrorCode = "invalid_message"
	CodeWrongRole          ErrorCode = "wrong_role"
	CodeWrongPhase         ErrorCode = "wrong_phase"
	CodeDuplicateOffer     ErrorCode = "duplicate_offer"
	CodeCandidateBanned    ErrorCode = "candidate_banned"
	CodeCandidateMalformed ErrorCode = "candidate_malformed"
	CodePeerUnavailable    ErrorCode = "peer_unavailable"
	CodeRoleTaken          ErrorCode = "role_taken"
	CodeHeartbeatTimeout   ErrorCode = "heartbeat_timeout"
	CodeSessionClosed      ErrorCode = "session_closed"
)

var (
	// ErrInvalidMessage is returned for any malformed or phase-invalid input.
	ErrInvalidMessage = errors.New("signal: invalid message")
	// ErrNonLoopbackCandidate is a policy violation, not just malformed data.
	ErrNonLoopbackCandidate = errors.New("signal: non-loopback candidate")
)

// ICECandidate mirrors the standard trickle ICE JSON shape without tying callers
// to a particular WebRTC library.
type ICECandidate struct {
	Candidate     string  `json:"candidate"`
	SDPMid        *string `json:"sdp_mid,omitempty"`
	SDPMLineIndex *uint16 `json:"sdp_mline_index,omitempty"`
}

// Message is one line of the signalling protocol.
type Message struct {
	Type      MessageType   `json:"type"`
	SessionID string        `json:"session_id,omitempty"`
	Role      Role          `json:"role,omitempty"`
	SDP       string        `json:"sdp,omitempty"`
	Candidate *ICECandidate `json:"candidate,omitempty"`
	Code      ErrorCode     `json:"code,omitempty"`
	Message   string        `json:"message,omitempty"`
}

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// Validate checks the shape and role/phase rules that can be enforced without
// session state. Stateful rules such as duplicate offers live in Server.
func Validate(m *Message) error {
	if m == nil {
		return fmt.Errorf("%w: nil", ErrInvalidMessage)
	}
	if m.Type == TypeError {
		if m.Code == "" || m.Message == "" {
			return fmt.Errorf("%w: incomplete error", ErrInvalidMessage)
		}
		return nil
	}
	if !sessionIDPattern.MatchString(m.SessionID) {
		return fmt.Errorf("%w: invalid session_id", ErrInvalidMessage)
	}
	if m.Type == TypeReady {
		if m.SDP != "" || m.Candidate != nil || m.Role != "" {
			return fmt.Errorf("%w: invalid ready message", ErrInvalidMessage)
		}
		return nil
	}
	if m.Role != RoleSender && m.Role != RoleRelay {
		return fmt.Errorf("%w: invalid role %q", ErrInvalidMessage, m.Role)
	}

	switch m.Type {
	case TypeHello:
		if m.SDP != "" || m.Candidate != nil {
			return fmt.Errorf("%w: hello carries media state", ErrInvalidMessage)
		}
	case TypeOffer, TypeAnswer:
		if err := validateSDP(m.SDP, m.Type); err != nil {
			return err
		}
		if m.Candidate != nil {
			return fmt.Errorf("%w: SDP message carries a candidate", ErrInvalidMessage)
		}
	case TypeCandidate:
		if m.Candidate == nil {
			return fmt.Errorf("%w: missing candidate", ErrInvalidMessage)
		}
		if err := validateCandidate(m.Candidate); err != nil {
			return err
		}
		if m.SDP != "" {
			return fmt.Errorf("%w: candidate carries SDP", ErrInvalidMessage)
		}
	case TypeBye, TypeHeartbeat:
		if m.SDP != "" || m.Candidate != nil {
			return fmt.Errorf("%w: %s carries media state", ErrInvalidMessage, m.Type)
		}
	default:
		return fmt.Errorf("%w: unknown type %q", ErrInvalidMessage, m.Type)
	}
	return nil
}

func validateSDP(sdp string, msgType MessageType) error {
	if len(sdp) == 0 || len(sdp) > 1<<20 {
		return fmt.Errorf("%w: bad SDP length", ErrInvalidMessage)
	}
	normalized := strings.ReplaceAll(sdp, "\r\n", "\n")
	if !strings.HasPrefix(normalized, "v=0\n") || !strings.Contains(normalized, "\nm=") {
		return fmt.Errorf("%w: malformed %s SDP", ErrInvalidMessage, msgType)
	}
	switch msgType {
	case TypeOffer:
		if !strings.Contains(normalized, "a=setup:actpass") && !strings.Contains(normalized, "a=setup:active") && !strings.Contains(normalized, "a=setup:passive") {
			return fmt.Errorf("%w: offer lacks DTLS setup", ErrInvalidMessage)
		}
	case TypeAnswer:
		if !strings.Contains(normalized, "a=setup:active") && !strings.Contains(normalized, "a=setup:passive") {
			return fmt.Errorf("%w: answer lacks DTLS setup", ErrInvalidMessage)
		}
	}
	return nil
}

func validateCandidate(c *ICECandidate) error {
	if c.Candidate == "" || len(c.Candidate) > 4096 {
		return fmt.Errorf("%w: bad candidate length", ErrInvalidMessage)
	}
	cand, err := ice.UnmarshalCandidate(c.Candidate)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidMessage, err)
	}
	if cand.Port() <= 0 || cand.Port() > 65535 {
		return fmt.Errorf("%w: bad candidate port", ErrInvalidMessage)
	}
	ip := net.ParseIP(cand.Address())
	if ip == nil {
		return fmt.Errorf("%w: bad candidate address", ErrInvalidMessage)
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("%w: candidate address %s", ErrNonLoopbackCandidate, cand.Address())
	}
	return nil
}

// Reader decodes messages from a long-lived newline-delimited stream.
type Reader struct {
	scanner *bufio.Scanner
}

// NewReader creates a protocol reader with a bounded line size.
func NewReader(r io.Reader) *Reader {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	return &Reader{scanner: scanner}
}

// Read returns the next message in the stream.
func (r *Reader) Read() (*Message, error) {
	if !r.scanner.Scan() {
		if err := r.scanner.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	raw := bytes.TrimSpace(r.scanner.Bytes())
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: empty line", ErrInvalidMessage)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var m Message
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidMessage, err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("%w: trailing JSON", ErrInvalidMessage)
	}
	return &m, nil
}

// ReadMessage reads one JSON object from r. Use Reader when reading multiple
// messages from the same stream, because bufio.Scanner may read ahead.
func ReadMessage(r io.Reader) (*Message, error) {
	return NewReader(r).Read()
}

// WriteMessage writes one compact JSON object followed by a newline.
func WriteMessage(w io.Writer, m *Message) error {
	if m == nil {
		return ErrInvalidMessage
	}
	enc, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = w.Write(append(enc, '\n'))
	return err
}

// ProtocolError is the wire representation of a rejected request.
type ProtocolError struct {
	Code    ErrorCode
	Message string
}

func (e *ProtocolError) Error() string { return string(e.Code) + ": " + e.Message }

func protocolError(code ErrorCode, err error) *ProtocolError {
	return &ProtocolError{Code: code, Message: err.Error()}
}
