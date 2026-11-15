package signal

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"pairwise.local/relayctl/internal/guard"
)

const (
	maxMessageBytes  = 1 << 20
	maxSubscribers   = 8
	defaultHandshake = 5 * time.Second
)

// Stats is a point-in-time signaling server counter snapshot.
type Stats struct {
	Joins             uint64 `json:"joins"`
	Rejected          uint64 `json:"rejected"`
	Forwarded         uint64 `json:"forwarded"`
	HandshakeTimeouts uint64 `json:"handshake_timeouts"`
	ActiveEndpoints   int    `json:"active_endpoints"`
}

type roomState struct {
	publisher   *session
	subscribers map[string]*session
}

type session struct {
	server   *Server
	conn     net.Conn
	room     string
	role     string
	endpoint string
	peer     Peer
	writeMu  sync.Mutex
	outSeq   uint64
	inSeq    uint64
	ready    atomic.Bool
	closed   atomic.Bool
	timer    *time.Timer
}

// Server owns rooms and relays SDP/ICE between clients and an Engine.
type Server struct {
	engine            Engine
	handshakeTimeout  time.Duration
	listener          net.Listener
	listenerMu        sync.Mutex
	mu                sync.Mutex
	rooms             map[string]*roomState
	sessions          map[string]*session
	nextEndpoint      uint64
	joins             atomic.Uint64
	rejected          atomic.Uint64
	forwarded         atomic.Uint64
	handshakeTimeouts atomic.Uint64
	closeOnce         sync.Once
	fatalOnce         sync.Once
	fatal             chan error
	fatalErr          atomic.Value
}

// NewServer constructs a signaling server around a media engine.
func NewServer(engine Engine) *Server {
	return &Server{
		engine:           engine,
		handshakeTimeout: defaultHandshake,
		rooms:            make(map[string]*roomState),
		sessions:         make(map[string]*session),
		fatal:            make(chan error, 1),
	}
}

// SetHandshakeTimeout primarily supports deterministic tests.
func (s *Server) SetHandshakeTimeout(timeout time.Duration) {
	if timeout > 0 {
		s.handshakeTimeout = timeout
	}
}

// Listen validates and binds the TCP listener. Only 127.0.0.0/8 is accepted.
func (s *Server) Listen(address string) error {
	if err := guard.ValidateListenAddress(address); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	s.listenerMu.Lock()
	s.listener = listener
	s.listenerMu.Unlock()
	return nil
}

// Addr returns the bound address after Listen.
func (s *Server) Addr() net.Addr {
	s.listenerMu.Lock()
	defer s.listenerMu.Unlock()
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

// Fatal exposes fatal policy violations such as non-loopback ICE candidates.
func (s *Server) Fatal() <-chan error { return s.fatal }

// Serve accepts clients until ctx cancellation or Close.
func (s *Server) Serve(ctx context.Context) error {
	s.listenerMu.Lock()
	listener := s.listener
	s.listenerMu.Unlock()
	if listener == nil {
		return fmt.Errorf("signal server is not listening")
	}
	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if fatal, ok := s.fatalErr.Load().(error); ok {
				return fatal
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				return err
			}
		}
		go s.serveConn(conn)
	}
}

// Close stops the listener and all active sessions.
func (s *Server) Close() error {
	var first error
	s.closeOnce.Do(func() {
		s.listenerMu.Lock()
		listener := s.listener
		s.listenerMu.Unlock()
		if listener != nil {
			first = listener.Close()
		}
		s.mu.Lock()
		sessions := make([]*session, 0, len(s.sessions))
		for _, current := range s.sessions {
			sessions = append(sessions, current)
		}
		s.mu.Unlock()
		for _, current := range sessions {
			current.close()
		}
	})
	return first
}

func (s *Server) serveConn(conn net.Conn) {
	current := &session{server: s, conn: conn}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), maxMessageBytes)
	defer current.close()
	for scanner.Scan() {
		var message Message
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			s.rejected.Add(1)
			current.sendError(CodeBadMessage, "invalid JSON")
			continue
		}
		if message.Type != TypeJoin && current.endpoint == "" {
			s.rejected.Add(1)
			current.sendError(CodeBadMessage, "join required first")
			continue
		}
		if message.Seq <= current.inSeq {
			s.rejected.Add(1)
			current.sendError(CodeSequence, "duplicate or regressed sequence")
			continue
		}
		current.inSeq = message.Seq
		if message.Type == TypeJoin {
			if err := s.handleJoin(current, message); err != nil {
				s.rejected.Add(1)
				current.sendError(CodeBadMessage, err.Error())
			}
			continue
		}
		if err := s.handleSignal(current, message); err != nil {
			s.rejected.Add(1)
			current.sendError(CodeBadMessage, err.Error())
			continue
		}
		s.forwarded.Add(1)
		if !current.ready.Load() && current.timer != nil {
			current.timer.Reset(s.handshakeTimeout)
		}
	}
}

func (s *Server) handleJoin(current *session, message Message) error {
	if current.endpoint != "" {
		return fmt.Errorf("connection already joined")
	}
	if message.Room == "" {
		return fmt.Errorf("room is required")
	}
	if message.Role != RolePublisher && message.Role != RoleSubscriber {
		return fmt.Errorf("role must be publisher or subscriber")
	}
	s.mu.Lock()
	room := s.rooms[message.Room]
	if room == nil {
		room = &roomState{subscribers: make(map[string]*session)}
		s.rooms[message.Room] = room
	}
	endpoint := message.Endpoint
	if endpoint == "" {
		s.nextEndpoint++
		prefix := "s"
		if message.Role == RolePublisher {
			prefix = "p"
		}
		endpoint = fmt.Sprintf("%s-%d", prefix, s.nextEndpoint)
	}
	old := s.sessions[endpoint]
	if old != nil {
		if old.role != message.Role || old.room != message.Room {
			s.mu.Unlock()
			return fmt.Errorf("endpoint resume does not match role or room")
		}
	}
	if message.Role == RolePublisher {
		if room.publisher != nil && room.publisher != current && room.publisher != old {
			s.mu.Unlock()
			return fmt.Errorf("room already has a publisher")
		}
	} else if old == nil && len(room.subscribers) >= maxSubscribers {
		s.mu.Unlock()
		return fmt.Errorf("subscriber capacity reached")
	}
	current.room, current.role, current.endpoint = message.Room, message.Role, endpoint
	s.sessions[endpoint] = current
	if message.Role == RolePublisher {
		room.publisher = current
	} else {
		room.subscribers[endpoint] = current
	}
	s.mu.Unlock()
	if old != nil && old != current {
		old.close()
	}
	peer, err := s.newPeer(message.Role, message.Room, endpoint)
	if err != nil {
		current.close()
		return err
	}
	current.peer = peer
	peer.OnLocalCandidate(func(candidate Candidate) {
		if err := guard.ValidateCandidate(candidate.Candidate); err != nil {
			s.fail(fmt.Errorf("%w: %v", guard.ErrNonLoopback, err))
			current.close()
			return
		}
		_ = current.send(Message{Type: TypeCandidate, Candidate: &candidate})
	})
	s.joins.Add(1)
	current.timer = time.AfterFunc(s.handshakeTimeout, func() {
		if current.ready.Load() || current.closed.Load() {
			return
		}
		s.handshakeTimeouts.Add(1)
		current.sendError(CodeHandshakeTimeout, "SDP/ICE handshake did not complete")
		current.close()
	})
	if err := current.send(Message{Type: TypeJoined, Room: message.Room, Role: message.Role, Endpoint: endpoint}); err != nil {
		return err
	}
	if message.Role == RoleSubscriber {
		offer, err := peer.CreateOffer()
		if err != nil {
			return fmt.Errorf("create offer: %w", err)
		}
		if err := current.send(Message{Type: TypeOffer, SDP: offer}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) newPeer(role, room, endpoint string) (Peer, error) {
	if role == RolePublisher {
		return s.engine.NewPublisher(room, endpoint)
	}
	return s.engine.NewSubscriber(room, endpoint)
}

func (s *Server) handleSignal(current *session, message Message) error {
	if current.peer == nil {
		return fmt.Errorf("unknown session")
	}
	switch message.Type {
	case TypeOffer:
		if current.role != RolePublisher {
			return fmt.Errorf("only publishers send offers")
		}
		if err := guard.ValidateSDP(message.SDP); err != nil {
			s.fail(fmt.Errorf("%w: %v", guard.ErrNonLoopback, err))
			return err
		}
		answer, err := current.peer.AcceptOffer(message.SDP)
		if err != nil {
			return err
		}
		if err := current.send(Message{Type: TypeAnswer, SDP: answer}); err != nil {
			return err
		}
		current.markReady()
	case TypeAnswer:
		if current.role != RoleSubscriber {
			return fmt.Errorf("only subscribers send answers")
		}
		if err := guard.ValidateSDP(message.SDP); err != nil {
			s.fail(fmt.Errorf("%w: %v", guard.ErrNonLoopback, err))
			return err
		}
		if err := current.peer.AcceptAnswer(message.SDP); err != nil {
			return err
		}
		current.markReady()
	case TypeCandidate:
		if message.Candidate == nil {
			return fmt.Errorf("candidate is required")
		}
		if err := guard.ValidateCandidate(message.Candidate.Candidate); err != nil {
			s.fail(fmt.Errorf("%w: %v", guard.ErrNonLoopback, err))
			return err
		}
		return current.peer.AddICECandidate(*message.Candidate)
	case TypeBye:
		current.close()
		return nil
	default:
		return fmt.Errorf("unsupported message type %q", message.Type)
	}
	return nil
}

// Stats returns signaling counters.
func (s *Server) Stats() Stats {
	s.mu.Lock()
	active := len(s.sessions)
	s.mu.Unlock()
	return Stats{
		Joins:             s.joins.Load(),
		Rejected:          s.rejected.Load(),
		Forwarded:         s.forwarded.Load(),
		HandshakeTimeouts: s.handshakeTimeouts.Load(),
		ActiveEndpoints:   active,
	}
}

func (s *Server) fail(err error) {
	s.fatalOnce.Do(func() {
		s.fatalErr.Store(err)
		s.fatal <- err
		s.listenerMu.Lock()
		listener := s.listener
		s.listenerMu.Unlock()
		if listener != nil {
			_ = listener.Close()
		}
	})
}

func (current *session) send(message Message) error {
	if current.closed.Load() {
		return net.ErrClosed
	}
	current.writeMu.Lock()
	defer current.writeMu.Unlock()
	current.outSeq++
	message.Seq = current.outSeq
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = current.conn.Write(data)
	return err
}

func (current *session) sendError(code, text string) {
	_ = current.send(Message{Type: TypeError, Code: code, Error: text})
}

func (current *session) markReady() {
	current.ready.Store(true)
	if current.timer != nil {
		current.timer.Stop()
	}
}

func (current *session) close() {
	if !current.closed.CompareAndSwap(false, true) {
		return
	}
	if current.timer != nil {
		current.timer.Stop()
	}
	_ = current.conn.Close()
	server := current.server
	if current.endpoint == "" {
		return
	}
	server.mu.Lock()
	if server.sessions[current.endpoint] == current {
		delete(server.sessions, current.endpoint)
	}
	room := server.rooms[current.room]
	if room != nil {
		if room.publisher == current {
			room.publisher = nil
		}
		if room.subscribers[current.endpoint] == current {
			delete(room.subscribers, current.endpoint)
		}
		if room.publisher == nil && len(room.subscribers) == 0 {
			delete(server.rooms, current.room)
		}
	}
	server.mu.Unlock()
	if current.peer != nil {
		_ = current.peer.Close()
	}
	server.engine.RemovePeer(current.room, current.endpoint)
}
