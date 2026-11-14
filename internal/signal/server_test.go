package signal

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func receiveMessage(t *testing.T, client *Client, want MessageType) *Message {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", want)
		case err := <-client.Errors():
			t.Fatalf("client error: %v", err)
		case m, ok := <-client.Messages():
			if !ok {
				t.Fatalf("messages closed waiting for %s", want)
			}
			if m.Type == want {
				return m
			}
		}
	}
}

func receiveErrorCode(t *testing.T, client *Client, want ErrorCode) *Message {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for error %s", want)
		case err := <-client.Errors():
			t.Fatalf("client error: %v", err)
		case m, ok := <-client.Messages():
			if !ok {
				t.Fatalf("messages closed waiting for error %s", want)
			}
			if m.Type == TypeError && m.Code == want {
				return m
			}
		}
	}
}

func TestServerPairsRolesAndEnforcesState(t *testing.T) {
	server, err := Listen("127.0.0.1:0", Options{HeartbeatTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	relayClient, err := Dial(ctx, server.Addr().String(), "state-test", RoleRelay, ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer relayClient.Close()
	senderClient, err := Dial(ctx, server.Addr().String(), "state-test", RoleSender, ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer senderClient.Close()
	receiveMessage(t, relayClient, TypeReady)
	receiveMessage(t, senderClient, TypeReady)

	if err := senderClient.Send(&Message{Type: TypeOffer, SDP: "v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 96\r\n"}); err != nil {
		t.Fatal(err)
	}
	receiveErrorCode(t, senderClient, CodeInvalidMessage)

	loopback := &ICECandidate{Candidate: "candidate:1 1 UDP 2122260223 127.0.0.1 5000 typ host"}
	if err := senderClient.Send(&Message{Type: TypeCandidate, Candidate: loopback}); err != nil {
		t.Fatal(err)
	}
	candidate := receiveMessage(t, relayClient, TypeCandidate)
	if candidate.Candidate == nil || candidate.Candidate.Candidate != loopback.Candidate {
		t.Fatalf("candidate mismatch: %+v", candidate)
	}

	if err := senderClient.Send(&Message{Type: TypeOffer, SDP: testOfferSDP}); err != nil {
		t.Fatal(err)
	}
	offer := receiveMessage(t, relayClient, TypeOffer)
	if offer.SDP != testOfferSDP {
		t.Fatal("offer SDP was altered")
	}
	if err := relayClient.Send(&Message{Type: TypeAnswer, SDP: testAnswerSDP}); err != nil {
		t.Fatal(err)
	}
	receiveMessage(t, senderClient, TypeAnswer)

	if err := senderClient.Send(&Message{Type: TypeOffer, SDP: testOfferSDP}); err != nil {
		t.Fatal(err)
	}
	receiveErrorCode(t, senderClient, CodeDuplicateOffer)
	if err := senderClient.Send(&Message{Type: TypeAnswer, SDP: testAnswerSDP}); err != nil {
		t.Fatal(err)
	}
	receiveErrorCode(t, senderClient, CodeWrongRole)

	bad := &ICECandidate{Candidate: "candidate:2 1 UDP 2122260223 192.0.2.10 5001 typ host"}
	if err := senderClient.Send(&Message{Type: TypeCandidate, Candidate: bad}); err != nil {
		t.Fatal(err)
	}
	receiveErrorCode(t, senderClient, CodeCandidateBanned)
	if server.BannedCandidates() != 1 {
		t.Fatalf("banned = %d, want 1", server.BannedCandidates())
	}
}

func TestClientReconnectWindowTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = Dial(ctx, addr, "timeout-test", RoleSender, ClientOptions{ReconnectWindow: 200 * time.Millisecond})
	if !errors.Is(err, ErrSignalTimeout) {
		t.Fatalf("error = %v, want ErrSignalTimeout", err)
	}
}

func TestServerRejectsNonLoopbackBind(t *testing.T) {
	if _, err := Listen("0.0.0.0:0", Options{}); !errors.Is(err, ErrNonLoopbackCandidate) {
		t.Fatalf("error = %v, want loopback policy", err)
	}
}

func TestServerReapsIdleSession(t *testing.T) {
	server, err := Listen("127.0.0.1:0", Options{HeartbeatTimeout: 120 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := Dial(ctx, server.Addr().String(), "idle-test", RoleSender, ClientOptions{HeartbeatInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	receiveErrorCode(t, client, CodeHeartbeatTimeout)
}

func TestClientsReconnectAfterServerDropsSession(t *testing.T) {
	server, err := Listen("127.0.0.1:0", Options{HeartbeatTimeout: 120 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	options := ClientOptions{HeartbeatInterval: time.Hour}
	relayClient, err := Dial(ctx, server.Addr().String(), "reconnect-test", RoleRelay, options)
	if err != nil {
		t.Fatal(err)
	}
	defer relayClient.Close()
	senderClient, err := Dial(ctx, server.Addr().String(), "reconnect-test", RoleSender, options)
	if err != nil {
		t.Fatal(err)
	}
	defer senderClient.Close()
	receiveMessage(t, relayClient, TypeReady)
	receiveMessage(t, senderClient, TypeReady)
	// Both clients reconnect automatically after the idle-session drop and
	// receive a fresh ready event with no caller intervention.
	receiveMessage(t, relayClient, TypeReady)
	receiveMessage(t, senderClient, TypeReady)
}
