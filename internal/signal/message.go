// Package signal implements relayctl's line-delimited JSON signaling.
package signal

import (
	"errors"
)

const (
	TypeJoin      = "join"
	TypeJoined    = "joined"
	TypeOffer     = "offer"
	TypeAnswer    = "answer"
	TypeCandidate = "candidate"
	TypeBye       = "bye"
	TypeError     = "error"

	RolePublisher  = "publisher"
	RoleSubscriber = "subscriber"

	CodeBadMessage       = "bad_message"
	CodeSequence         = "sequence"
	CodeHandshakeTimeout = "handshake_timeout"
	CodeNonLoopback      = "non_loopback"
	CodeCapacity         = "capacity"
	CodeInternal         = "internal"
)

// Message is one signaling frame. Seq is strictly increasing per sender.
type Message struct {
	Type      string     `json:"type"`
	Seq       uint64     `json:"seq"`
	Room      string     `json:"room,omitempty"`
	Role      string     `json:"role,omitempty"`
	Endpoint  string     `json:"endpoint,omitempty"`
	SDP       string     `json:"sdp,omitempty"`
	Candidate *Candidate `json:"candidate,omitempty"`
	Code      string     `json:"code,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// Candidate mirrors webrtc.ICECandidateInit without coupling signaling to Pion.
type Candidate struct {
	Candidate     string  `json:"candidate"`
	SDPMid        *string `json:"sdpMid,omitempty"`
	SDPMLineIndex *uint16 `json:"sdpMLineIndex,omitempty"`
}

// Peer is the media-side half of one publisher or subscriber connection.
type Peer interface {
	CreateOffer() (string, error)
	AcceptOffer(string) (string, error)
	AcceptAnswer(string) error
	AddICECandidate(Candidate) error
	OnLocalCandidate(func(Candidate))
	Close() error
}

// Engine allocates media peers. It is implemented by relay.Engine.
type Engine interface {
	NewPublisher(room, endpoint string) (Peer, error)
	NewSubscriber(room, endpoint string) (Peer, error)
	RemovePeer(room, endpoint string)
}

// RemoteError is returned by clients when the server rejects a request.
type RemoteError struct {
	Code    string
	Message string
}

func (e *RemoteError) Error() string { return e.Code + ": " + e.Message }

// IsCode reports whether err is a RemoteError with the requested code.
func IsCode(err error, code string) bool {
	var remote *RemoteError
	return errors.As(err, &remote) && remote.Code == code
}
