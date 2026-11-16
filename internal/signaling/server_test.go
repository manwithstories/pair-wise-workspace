package signaling

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func startServer(t *testing.T, idle time.Duration) *Server {
	t.Helper()
	srv, err := Listen("127.0.0.1:0", idle)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func dial(t *testing.T, srv *Server) *Client {
	t.Helper()
	c, err := Dial(srv.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func mustRecv(t *testing.T, c *Client) Message {
	t.Helper()
	m, err := c.Recv(time.Now().Add(2 * time.Second))
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	return m
}

func openSession(t *testing.T, srv *Server) (*Client, *Client, string) {
	t.Helper()
	offerer := dial(t, srv)
	if err := offerer.Send(Message{Type: KindOffer, SDP: "v=0 offer"}); err != nil {
		t.Fatalf("send offer: %v", err)
	}
	ack := mustRecv(t, offerer)
	if ack.Type != KindState || ack.State != stateOfferAccepted || ack.Session == "" {
		t.Fatalf("unexpected offer ack: %+v", ack)
	}
	answerer := dial(t, srv)
	if err := answerer.Send(Message{Type: KindState, Session: ack.Session, State: stateJoin}); err != nil {
		t.Fatalf("join: %v", err)
	}
	if got := mustRecv(t, answerer); got.Type != KindOffer || got.SDP != "v=0 offer" {
		t.Fatalf("want relayed offer, got %+v", got)
	}
	if got := mustRecv(t, answerer); got.State != stateJoin {
		t.Fatalf("want join ack, got %+v", got)
	}
	return offerer, answerer, ack.Session
}

func TestOfferAnswerRoundTrip(t *testing.T) {
	srv := startServer(t, 2*time.Second)
	offerer, answerer, session := openSession(t, srv)

	if err := answerer.Send(Message{Type: KindAnswer, Session: session, SDP: "v=0 answer"}); err != nil {
		t.Fatalf("answer: %v", err)
	}
	got := mustRecv(t, offerer)
	if got.Type != KindAnswer || got.SDP != "v=0 answer" || got.Session != session {
		t.Fatalf("want relayed answer, got %+v", got)
	}
	if ack := mustRecv(t, answerer); ack.State != stateAnswerAccepted {
		t.Fatalf("want answer ack, got %+v", ack)
	}
	if st := srv.Stats(); st.SessionsCreated != 1 || st.MessagesForwarded < 3 {
		t.Fatalf("unexpected server stats: %+v", st)
	}
}

func TestCandidateRelayBothDirections(t *testing.T) {
	srv := startServer(t, 2*time.Second)
	offerer, answerer, session := openSession(t, srv)

	cand := "candidate:1 1 udp 2130706431 127.0.0.1 5000 typ host"
	if err := offerer.Send(Message{Type: KindCandidate, Session: session, Candidate: cand}); err != nil {
		t.Fatalf("send candidate: %v", err)
	}
	if got := mustRecv(t, answerer); got.Candidate != cand {
		t.Fatalf("candidate not relayed: %+v", got)
	}
	ipv6 := "candidate:2 1 udp 2130706431 ::1 5001 typ host"
	if err := answerer.Send(Message{Type: KindCandidate, Session: session, Candidate: ipv6}); err != nil {
		t.Fatalf("send candidate: %v", err)
	}
	if got := mustRecv(t, offerer); got.Candidate != ipv6 {
		t.Fatalf("IPv6 candidate not relayed: %+v", got)
	}
}

func TestCandidateBeforeAnswererJoinsIsBuffered(t *testing.T) {
	srv := startServer(t, 2*time.Second)
	offerer := dial(t, srv)
	if err := offerer.Send(Message{Type: KindOffer, SDP: "offer"}); err != nil {
		t.Fatal(err)
	}
	session := mustRecv(t, offerer).Session

	cand := "candidate:1 1 udp 100 127.0.0.1 6000 typ host"
	if err := offerer.Send(Message{Type: KindCandidate, Session: session, Candidate: cand}); err != nil {
		t.Fatalf("early candidate: %v", err)
	}

	answerer := dial(t, srv)
	if err := answerer.Send(Message{Type: KindState, Session: session, State: stateJoin}); err != nil {
		t.Fatal(err)
	}
	_ = mustRecv(t, answerer) // offer
	_ = mustRecv(t, answerer) // join ack
	if got := mustRecv(t, answerer); got.Candidate != cand {
		t.Fatalf("buffered candidate was not flushed: %+v", got)
	}
}

func TestNonLoopbackCandidateRejectedAndRecorded(t *testing.T) {
	srv := startServer(t, 2*time.Second)
	offerer, _, session := openSession(t, srv)

	bad := "candidate:1 1 udp 100 10.1.2.3 7000 typ host"
	if err := offerer.Send(Message{Type: KindCandidate, Session: session, Candidate: bad}); err != nil {
		t.Fatal(err)
	}
	got := mustRecv(t, offerer)
	if got.Type != KindError || got.Code != ErrCodeNonLoopback {
		t.Fatalf("want non_loopback rejection, got %+v", got)
	}
	st := srv.Stats()
	if st.RejectedByCode[ErrCodeNonLoopback] != 1 {
		t.Fatalf("rejection not counted by code: %+v", st.RejectedByCode)
	}
	if len(st.Rejections) == 0 || !strings.Contains(st.Rejections[0].Detail, "10.1.2.3") {
		t.Fatalf("rejection reason not recorded: %+v", st.Rejections)
	}
}

func TestMalformedJSONRejected(t *testing.T) {
	srv := startServer(t, 2*time.Second)
	c := dial(t, srv)
	if _, err := c.conn.Write([]byte("{not json}\n")); err != nil {
		t.Fatal(err)
	}
	got := mustRecv(t, c)
	if got.Type != KindError || got.Code != ErrCodeInvalidJSON {
		t.Fatalf("want invalid_json, got %+v", got)
	}
	// The connection must survive a bad message.
	if err := c.Send(Message{Type: KindOffer, SDP: "still works"}); err != nil {
		t.Fatalf("server closed after a bad message: %v", err)
	}
	if ack := mustRecv(t, c); ack.State != stateOfferAccepted {
		t.Fatalf("server did not recover from bad JSON: %+v", ack)
	}
}

func TestUnknownSessionRejected(t *testing.T) {
	srv := startServer(t, 2*time.Second)
	c := dial(t, srv)
	if err := c.Send(Message{Type: KindAnswer, Session: "nope", SDP: "x"}); err != nil {
		t.Fatal(err)
	}
	got := mustRecv(t, c)
	if got.Type != KindError || got.Code != ErrCodeUnknownSession {
		t.Fatalf("want unknown_session, got %+v", got)
	}
}

func TestDuplicateSequenceRejected(t *testing.T) {
	srv := startServer(t, 2*time.Second)
	offerer, _, session := openSession(t, srv)

	if err := offerer.Send(Message{Type: KindCandidate, Session: session, Candidate: "candidate:1 1 udp 1 127.0.0.1 1 typ host"}); err != nil {
		t.Fatal(err)
	}
	// Replay the same sequence number deliberately.
	if err := offerer.SendRaw(Message{Type: KindCandidate, Seq: 2, Session: session, Candidate: "candidate:1 1 udp 1 127.0.0.1 2 typ host"}); err != nil {
		t.Fatal(err)
	}
	got := mustRecv(t, offerer)
	if got.Type != KindError || got.Code != ErrCodeDuplicate {
		t.Fatalf("want duplicate_message, got %+v", got)
	}
}

func TestOutOfOrderSequenceRejected(t *testing.T) {
	srv := startServer(t, 2*time.Second)
	offerer, _, session := openSession(t, srv)
	// Send one candidate normally (seq 2), then send a lower sequence number.
	// seq 1 cannot be replayed here because the offer did not register one.
	if err := offerer.Send(Message{Type: KindCandidate, Session: session, Candidate: "candidate:1 1 udp 1 127.0.0.1 1 typ host"}); err != nil {
		t.Fatal(err)
	}
	if err := offerer.SendRaw(Message{Type: KindCandidate, Seq: 1, Session: session, Candidate: "candidate:1 1 udp 1 127.0.0.1 2 typ host"}); err != nil {
		t.Fatal(err)
	}
	got := mustRecv(t, offerer)
	if got.Type != KindError || got.Code != ErrCodeOutOfOrder {
		t.Fatalf("want out_of_order_message, got %+v", got)
	}
}

func TestIdleSessionExpires(t *testing.T) {
	srv, err := Listen("127.0.0.1:0", 120*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	c := dial(t, srv)

	if err := c.Send(Message{Type: KindOffer, SDP: "offer"}); err != nil {
		t.Fatal(err)
	}
	_ = mustRecv(t, c) // ack
	msg, err := c.Recv(time.Now().Add(2 * time.Second))
	if err != nil {
		t.Fatalf("expected a timeout error message, got read error %v", err)
	}
	if msg.Type != KindError || msg.Code != ErrCodeTimeout {
		t.Fatalf("want signaling_timeout, got %+v", msg)
	}
	if st := srv.Stats(); st.SessionsExpired != 1 {
		t.Fatalf("expiry not counted: %+v", st)
	}
}

func TestPeerCloseNotifiesOtherSide(t *testing.T) {
	srv := startServer(t, 5*time.Second)
	offerer, answerer, _ := openSession(t, srv)
	_ = offerer.Close()

	msg, err := answerer.Recv(time.Now().Add(2 * time.Second))
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if msg.Type != KindState || msg.State != statePeerClosed {
		t.Fatalf("want peer_closed, got %+v", msg)
	}
}

func TestServerCloseReleasesConnections(t *testing.T) {
	srv := startServer(t, 5*time.Second)
	c := dial(t, srv)
	if err := c.Send(Message{Type: KindOffer, SDP: "offer"}); err != nil {
		t.Fatal(err)
	}
	_ = mustRecv(t, c)
	if err := srv.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := c.Recv(time.Now().Add(time.Second)); err == nil {
		t.Fatal("connection should be closed after server shutdown")
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("second close should be a no-op: %v", err)
	}
}

func TestProtocolErrorCodesAreStable(t *testing.T) {
	err := newProtocolError(ErrCodeSessionMismatch, "x")
	if ErrCode(err) != ErrCodeSessionMismatch {
		t.Fatalf("ErrCode lost the code: %v", ErrCode(err))
	}
	if ErrCode(errors.New("plain")) != "" {
		t.Fatal("ErrCode should return empty for non-protocol errors")
	}
}
