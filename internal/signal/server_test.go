package signal

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"
)

type fakePeer struct {
	mu       sync.Mutex
	answer   string
	offer    string
	onLocal  func(Candidate)
	closed   bool
	addedICE int
}

func (p *fakePeer) CreateOffer() (string, error) {
	return "v=0\r\na=candidate:1 1 udp 1 127.0.0.1 50000 typ host\r\n", nil
}
func (p *fakePeer) AcceptOffer(string) (string, error) {
	return "v=0\r\na=candidate:1 1 udp 1 127.0.0.1 50001 typ host\r\n", nil
}
func (p *fakePeer) AcceptAnswer(string) error { return nil }
func (p *fakePeer) AddICECandidate(Candidate) error {
	p.mu.Lock()
	p.addedICE++
	p.mu.Unlock()
	return nil
}
func (p *fakePeer) OnLocalCandidate(fn func(Candidate)) { p.onLocal = fn }
func (p *fakePeer) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	return nil
}

type fakeEngine struct {
	mu    sync.Mutex
	peers map[string]*fakePeer
}

func (e *fakeEngine) create(endpoint string) *fakePeer {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.peers == nil {
		e.peers = make(map[string]*fakePeer)
	}
	peer := &fakePeer{}
	e.peers[endpoint] = peer
	return peer
}
func (e *fakeEngine) NewPublisher(_ string, endpoint string) (Peer, error) {
	return e.create(endpoint), nil
}
func (e *fakeEngine) NewSubscriber(_ string, endpoint string) (Peer, error) {
	return e.create(endpoint), nil
}
func (e *fakeEngine) RemovePeer(_ string, endpoint string) {
	e.mu.Lock()
	delete(e.peers, endpoint)
	e.mu.Unlock()
}
func (e *fakeEngine) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.peers)
}

func startTestServer(t *testing.T, engine Engine) (*Server, string, context.CancelFunc) {
	t.Helper()
	server := NewServer(engine)
	if err := server.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = server.Serve(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		_ = server.Close()
		<-done
	})
	return server, server.Addr().String(), cancel
}

func TestPublisherAndSubscriberHandshake(t *testing.T) {
	engine := &fakeEngine{}
	server, address, _ := startTestServer(t, engine)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	publisher, joined, err := Dial(ctx, address, RolePublisher, "room")
	if err != nil {
		t.Fatal(err)
	}
	if joined.Endpoint == "" {
		t.Fatal("missing endpoint")
	}
	if err := publisher.Send(Message{Type: TypeOffer, SDP: "v=0\r\n"}); err != nil {
		t.Fatal(err)
	}
	if message, err := publisher.Recv(ctx); err != nil || message.Type != TypeAnswer {
		t.Fatalf("expected answer, got %+v, %v", message, err)
	}
	subscriber, _, err := Dial(ctx, address, RoleSubscriber, "room")
	if err != nil {
		t.Fatal(err)
	}
	offer, err := subscriber.Recv(ctx)
	if err != nil || offer.Type != TypeOffer {
		t.Fatalf("expected subscriber offer, got %+v, %v", offer, err)
	}
	if err := subscriber.Send(Message{Type: TypeAnswer, SDP: "v=0\r\n"}); err != nil {
		t.Fatal(err)
	}
	if engine.count() != 2 {
		t.Fatalf("expected two peers, got %d", engine.count())
	}
	deadline := time.Now().Add(time.Second)
	for server.Stats().Forwarded < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if server.Stats().Forwarded < 2 {
		t.Fatalf("forward counter too low: %+v", server.Stats())
	}
	_ = publisher.Close()
	_ = subscriber.Close()
}

func TestDuplicateSequenceRejected(t *testing.T) {
	engine := &fakeEngine{}
	server, address, _ := startTestServer(t, engine)
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	writer := bufio.NewWriter(conn)
	reader := bufio.NewReader(conn)
	send := func(message Message) {
		data, _ := json.Marshal(message)
		_, _ = writer.Write(append(data, '\n'))
		_ = writer.Flush()
	}
	read := func() Message {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var message Message
		if err := json.Unmarshal(line, &message); err != nil {
			t.Fatal(err)
		}
		return message
	}
	send(Message{Type: TypeJoin, Seq: 1, Room: "dupe", Role: RolePublisher})
	if joined := read(); joined.Type != TypeJoined {
		t.Fatalf("expected joined, got %+v", joined)
	}
	send(Message{Type: TypeJoin, Seq: 1, Room: "dupe", Role: RolePublisher})
	if rejected := read(); rejected.Type != TypeError || rejected.Code != CodeSequence {
		t.Fatalf("expected sequence rejection, got %+v", rejected)
	}
	if server.Stats().Rejected != 1 {
		t.Fatalf("expected one rejection: %+v", server.Stats())
	}
}

func TestHandshakeTimeout(t *testing.T) {
	engine := &fakeEngine{}
	server, address, _ := startTestServer(t, engine)
	server.SetHandshakeTimeout(25 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, _, err := Dial(ctx, address, RolePublisher, "slow")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Recv(ctx); !IsCode(err, CodeHandshakeTimeout) {
		t.Fatalf("expected handshake timeout, got %v", err)
	}
}

func TestNonLoopbackCandidateIsFatal(t *testing.T) {
	engine := &fakeEngine{}
	server, address, _ := startTestServer(t, engine)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, _, err := Dial(ctx, address, RolePublisher, "bad")
	if err != nil {
		t.Fatal(err)
	}
	offer := "v=0\r\na=candidate:1 1 udp 1 10.1.2.3 50000 typ host\r\n"
	if err := client.Send(Message{Type: TypeOffer, SDP: offer}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-server.Fatal():
		if err == nil {
			t.Fatal("expected fatal error")
		}
	case <-ctx.Done():
		t.Fatal("non-loopback candidate was not rejected")
	}
}

func TestReconnectResumesEndpoint(t *testing.T) {
	engine := &fakeEngine{}
	_, address, _ := startTestServer(t, engine)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, joined, err := Dial(ctx, address, RolePublisher, "resume")
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := client.Reconnect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Endpoint != joined.Endpoint {
		t.Fatalf("endpoint changed after reconnect: %s -> %s", joined.Endpoint, resumed.Endpoint)
	}
}

func TestResumeReplacesLivePublisherEndpoint(t *testing.T) {
	engine := &fakeEngine{}
	server, address, _ := startTestServer(t, engine)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	first, joined, err := Dial(ctx, address, RolePublisher, "resume-live")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	data, _ := json.Marshal(Message{Type: TypeJoin, Seq: 1, Room: "resume-live",
		Role: RolePublisher, Endpoint: joined.Endpoint})
	if _, err := conn.Write(append(data, '\n')); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var response Message
	if err := json.Unmarshal(line, &response); err != nil {
		t.Fatal(err)
	}
	if response.Type != TypeJoined || response.Endpoint != joined.Endpoint {
		t.Fatalf("publisher resume failed: %+v", response)
	}
	_ = first.Close()
	if server.Stats().Joins != 2 {
		t.Fatalf("unexpected joins: %+v", server.Stats())
	}
}
