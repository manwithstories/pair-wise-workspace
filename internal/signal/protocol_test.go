package signal

import (
	"errors"
	"io"
	"strings"
	"testing"
)

const testOfferSDP = "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 96\r\nc=IN IP4 127.0.0.1\r\na=setup:actpass\r\n"
const testAnswerSDP = "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 96\r\nc=IN IP4 127.0.0.1\r\na=setup:active\r\n"

func TestValidateMessageRolesAndSDP(t *testing.T) {
	valid := []Message{
		{Type: TypeHello, SessionID: "s-1", Role: RoleSender},
		{Type: TypeOffer, SessionID: "s-1", Role: RoleSender, SDP: testOfferSDP},
		{Type: TypeAnswer, SessionID: "s-1", Role: RoleRelay, SDP: testAnswerSDP},
		{Type: TypeHeartbeat, SessionID: "s-1", Role: RoleRelay},
	}
	for i := range valid {
		if err := Validate(&valid[i]); err != nil {
			t.Fatalf("valid message %d rejected: %v", i, err)
		}
	}
	cases := []struct {
		message Message
		want    error
	}{
		{Message{Type: TypeHello, SessionID: "bad id", Role: RoleSender}, ErrInvalidMessage},
		{Message{Type: TypeOffer, SessionID: "s-1", Role: RoleSender, SDP: "not sdp"}, ErrInvalidMessage},
		{Message{Type: TypeAnswer, SessionID: "s-1", Role: RoleRelay, SDP: "v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 96\r\n"}, ErrInvalidMessage},
		{Message{Type: TypeHello, SessionID: "s-1", Role: "viewer"}, ErrInvalidMessage},
	}
	for i, tc := range cases {
		if err := Validate(&tc.message); !errors.Is(err, tc.want) {
			t.Fatalf("case %d: got %v, want %v", i, err, tc.want)
		}
	}
}

func TestCandidateLoopbackPolicy(t *testing.T) {
	loopback := &ICECandidate{Candidate: "candidate:1 1 UDP 2122260223 127.0.0.1 5000 typ host"}
	msg := Message{Type: TypeCandidate, SessionID: "s-1", Role: RoleSender, Candidate: loopback}
	if err := Validate(&msg); err != nil {
		t.Fatalf("loopback candidate rejected: %v", err)
	}
	bad := &ICECandidate{Candidate: "candidate:2 1 UDP 2122260223 192.0.2.10 5000 typ host"}
	msg.Candidate = bad
	if err := Validate(&msg); !errors.Is(err, ErrNonLoopbackCandidate) {
		t.Fatalf("non-loopback candidate err = %v", err)
	}
	msg.Candidate = &ICECandidate{Candidate: "candidate:3 1 UDP 1 127.0.0.1 nope typ host"}
	if err := Validate(&msg); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("malformed candidate err = %v", err)
	}
}

func TestReadMessageNDJSON(t *testing.T) {
	input := "{\"type\":\"hello\",\"session_id\":\"s-1\",\"role\":\"sender\"}\n{\"type\":\"heartbeat\",\"session_id\":\"s-1\",\"role\":\"relay\"}\n"
	reader := NewReader(strings.NewReader(input))
	first, err := reader.Read()
	if err != nil || first.Type != TypeHello {
		t.Fatalf("first = %+v, err = %v", first, err)
	}
	second, err := reader.Read()
	if err != nil || second.Type != TypeHeartbeat {
		t.Fatalf("second = %+v, err = %v", second, err)
	}
	if _, err := reader.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func TestReadMessageRejectsMalformedJSON(t *testing.T) {
	cases := []string{
		"{\"type\":\"hello\",\"session_id\":\"s-1\",\"role\":\"sender\"} trailing\n",
		"{\"type\":\"hello\",\"session_id\":\"s-1\",\"role\":\"sender\",\"unknown\":1}\n",
		"\n",
	}
	for i, input := range cases {
		if _, err := ReadMessage(strings.NewReader(input)); !errors.Is(err, ErrInvalidMessage) {
			t.Fatalf("case %d error = %v", i, err)
		}
	}
}
