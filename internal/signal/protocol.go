package signal

import (
	"encoding/json"
	"fmt"

	"github.com/pion/webrtc/v4"
)

const ProtocolVersion = 1

type MessageType string

const (
	TypeHello     MessageType = "hello"
	TypeWelcome   MessageType = "welcome"
	TypeOffer     MessageType = "offer"
	TypeAnswer    MessageType = "answer"
	TypeTrickle   MessageType = "trickle"
	TypeHeartbeat MessageType = "heartbeat"
	TypeBye       MessageType = "bye"
)

type Envelope struct {
	Version int             `json:"version"`
	Seq     uint64          `json:"seq"`
	Type    MessageType     `json:"type"`
	Stream  string          `json:"stream"`
	Source  string          `json:"source,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type Hello struct {
	Role    string `json:"role"`
	Version int    `json:"version"`
}

type Welcome struct {
	Version int    `json:"version"`
	Stream  string `json:"stream"`
	Role    string `json:"role"`
}

type Offer struct {
	SDP string `json:"sdp"`
}

type Answer struct {
	SDP string `json:"sdp"`
}

type Trickle struct {
	Candidate *webrtc.ICECandidateInit `json:"candidate,omitempty"`
	End       bool                     `json:"end,omitempty"`
}

func ValidateEnvelope(env Envelope) error {
	if env.Version != ProtocolVersion {
		return fmt.Errorf("unsupported protocol version %d", env.Version)
	}
	switch env.Type {
	case TypeHello, TypeWelcome, TypeOffer, TypeAnswer, TypeTrickle, TypeHeartbeat, TypeBye:
		return nil
	default:
		return fmt.Errorf("unsupported message type %q", env.Type)
	}
}

func MarshalEnvelope(env Envelope) ([]byte, error) {
	if err := ValidateEnvelope(env); err != nil {
		return nil, err
	}
	return json.Marshal(env)
}

func UnmarshalEnvelope(data []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return Envelope{}, err
	}
	if err := ValidateEnvelope(env); err != nil {
		return Envelope{}, err
	}
	return env, nil
}
