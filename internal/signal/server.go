package signal

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultHeartbeatTimeout is the time after which an idle session is reclaimed.
const DefaultHeartbeatTimeout = 30 * time.Second

// Options controls server limits. Zero values select safe defaults.
type Options struct {
	HeartbeatTimeout time.Duration
	WriteTimeout     time.Duration
}

func (o Options) withDefaults() Options {
	if o.HeartbeatTimeout <= 0 {
		o.HeartbeatTimeout = DefaultHeartbeatTimeout
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = 5 * time.Second
	}
	return o
}

type sessionStage uint8

const (
	stagePaired sessionStage = iota
	stageOffered
	stageNegotiated
)

type connState struct {
	server *Server
	conn   net.Conn
	mu     sync.Mutex
	closed atomic.Bool
}

func (c *connState) send(m *Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() {
		return net.ErrClosed
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(c.server.options.WriteTimeout))
	return WriteMessage(c.conn, m)
}

func (c *connState) close() {
	if c.closed.CompareAndSwap(false, true) {
		_ = c.conn.Close()
	}
}

type session struct {
	id       string
	sender   *connState
	relay    *connState
	lastSeen time.Time
	stage    sessionStage
}

// Server brokers sender/relay sessions over loopback TCP. SDP and ICE are
// opaque payloads; the server only validates their shape and routing phase.
type Server struct {
	listener net.Listener
	options  Options
	ctx      context.Context
	cancel   context.CancelFunc
	closed   chan struct{}
	closeOne sync.Once
	wg       sync.WaitGroup

	mu       sync.Mutex
	sessions map[string]*session
	banned   atomic.Uint64
}

// Listen creates a loopback-only TCP server. Non-loopback bind addresses are
// rejected with ErrNonLoopbackCandidate.
func Listen(addr string, options Options) (*Server, error) {
	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("resolve signal bind: %w", err)
	}
	if tcpAddr.IP == nil || !tcpAddr.IP.IsLoopback() {
		return nil, fmt.Errorf("%w: signal bind %s", ErrNonLoopbackCandidate, addr)
	}
	ln, err := net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		return nil, fmt.Errorf("listen signal: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		listener: ln,
		options:  options.withDefaults(),
		ctx:      ctx,
		cancel:   cancel,
		closed:   make(chan struct{}),
		sessions: make(map[string]*session),
	}
	s.wg.Add(2)
	go s.acceptLoop()
	go s.reapLoop()
	return s, nil
}

// Addr returns the actual listener address, useful with port zero.
func (s *Server) Addr() net.Addr { return s.listener.Addr() }

// BannedCandidates counts candidates rejected by the loopback whitelist.
func (s *Server) BannedCandidates() uint64 { return s.banned.Load() }

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.ctx.Done():
				return
			default:
				continue
			}
		}
		s.wg.Add(1)
		go s.serveConn(conn)
	}
}

func (s *Server) reapLoop() {
	defer s.wg.Done()
	interval := s.options.HeartbeatTimeout / 4
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-ticker.C:
			s.reapIdle(now)
		}
	}
}

func (s *Server) reapIdle(now time.Time) {
	var expired []*connState
	s.mu.Lock()
	for id, sess := range s.sessions {
		if now.Sub(sess.lastSeen) <= s.options.HeartbeatTimeout {
			continue
		}
		delete(s.sessions, id)
		expired = append(expired, sess.sender, sess.relay)
	}
	s.mu.Unlock()
	for _, c := range expired {
		if c == nil {
			continue
		}
		_ = c.send(&Message{
			Type:    TypeError,
			Code:    CodeHeartbeatTimeout,
			Message: "session heartbeat timeout",
		})
		c.close()
	}
}

func (s *Server) serveConn(nc net.Conn) {
	defer s.wg.Done()
	c := &connState{server: s, conn: nc}
	defer c.close()
	reader := NewReader(nc)
	first, err := reader.Read()
	if err != nil {
		return
	}
	if err := Validate(first); err != nil {
		s.reject(c, first, err)
		return
	}
	if first.Type != TypeHello {
		_ = c.send(errorMessage(CodeWrongPhase, "first message must be hello"))
		return
	}
	peerReady, protoErr := s.join(first, c)
	if protoErr != nil {
		_ = c.send(errorMessage(protoErr.Code, protoErr.Message))
		return
	}
	if peerReady {
		s.sendReady(first.SessionID)
	}

	for {
		m, err := reader.Read()
		if err != nil {
			s.detach(first.SessionID, first.Role, c)
			return
		}
		if err := Validate(m); err != nil {
			s.reject(c, m, err)
			continue
		}
		if m.SessionID != first.SessionID || m.Role != first.Role {
			_ = c.send(errorMessage(CodeWrongRole, "message session/role does not match hello"))
			continue
		}
		if err := s.route(c, m); err != nil {
			_ = c.send(errorMessage(err.Code, err.Message))
		}
	}
}

func (s *Server) reject(c *connState, m *Message, err error) {
	code := CodeInvalidMessage
	if errors.Is(err, ErrNonLoopbackCandidate) {
		s.banned.Add(1)
		code = CodeCandidateBanned
	} else if m != nil && m.Type == TypeCandidate {
		code = CodeCandidateMalformed
	}
	_ = c.send(errorMessage(code, err.Error()))
}

func (s *Server) join(hello *Message, c *connState) (bool, *ProtocolError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	sess := s.sessions[hello.SessionID]
	if sess == nil {
		sess = &session{id: hello.SessionID, lastSeen: now, stage: stagePaired}
		s.sessions[hello.SessionID] = sess
	}
	var current **connState
	switch hello.Role {
	case RoleSender:
		current = &sess.sender
	case RoleRelay:
		current = &sess.relay
	default:
		return false, &ProtocolError{Code: CodeInvalidMessage, Message: "bad role"}
	}
	if *current != nil && !(*current).closed.Load() {
		return false, &ProtocolError{Code: CodeRoleTaken, Message: "role already connected"}
	}
	*current = c
	sess.lastSeen = now
	return sess.sender != nil && sess.relay != nil, nil
}

func (s *Server) detach(id string, role Role, c *connState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[id]
	if sess == nil {
		return
	}
	if role == RoleSender && sess.sender == c {
		sess.sender = nil
	}
	if role == RoleRelay && sess.relay == c {
		sess.relay = nil
	}
	sess.lastSeen = time.Now()
}

func (s *Server) sendReady(id string) {
	s.mu.Lock()
	sess := s.sessions[id]
	if sess == nil {
		s.mu.Unlock()
		return
	}
	conns := []*connState{sess.sender, sess.relay}
	s.mu.Unlock()
	for _, c := range conns {
		if c != nil {
			_ = c.send(&Message{Type: TypeReady, SessionID: id})
		}
	}
}

func (s *Server) route(c *connState, m *Message) *ProtocolError {
	now := time.Now()
	var sendTo *connState
	var outgoing *Message
	var closeSession bool

	s.mu.Lock()
	sess := s.sessions[m.SessionID]
	if sess == nil {
		s.mu.Unlock()
		return &ProtocolError{Code: CodeSessionClosed, Message: "session is not registered"}
	}
	sess.lastSeen = now
	if sess.sender == nil || sess.relay == nil {
		s.mu.Unlock()
		return &ProtocolError{Code: CodePeerUnavailable, Message: "peer is not connected"}
	}

	switch m.Type {
	case TypeOffer:
		if m.Role != RoleSender {
			s.mu.Unlock()
			return &ProtocolError{Code: CodeWrongRole, Message: "only sender may offer"}
		}
		if sess.stage >= stageOffered {
			s.mu.Unlock()
			return &ProtocolError{Code: CodeDuplicateOffer, Message: "offer already exists for session"}
		}
		sess.stage = stageOffered
		sendTo = sess.relay
		outgoing = m
	case TypeAnswer:
		if m.Role != RoleRelay {
			s.mu.Unlock()
			return &ProtocolError{Code: CodeWrongRole, Message: "only relay may answer"}
		}
		if sess.stage != stageOffered {
			s.mu.Unlock()
			return &ProtocolError{Code: CodeWrongPhase, Message: "answer requires a pending offer"}
		}
		sess.stage = stageNegotiated
		sendTo = sess.sender
		outgoing = m
	case TypeCandidate, TypeHeartbeat:
		if m.Role == RoleSender {
			sendTo = sess.relay
		} else {
			sendTo = sess.sender
		}
		outgoing = m
	case TypeBye:
		sendTo = sess.relay
		if m.Role == RoleRelay {
			sendTo = sess.sender
		}
		outgoing = m
		closeSession = true
		delete(s.sessions, m.SessionID)
	default:
		s.mu.Unlock()
		return &ProtocolError{Code: CodeWrongPhase, Message: "unsupported message type"}
	}
	s.mu.Unlock()

	if sendTo == nil {
		return &ProtocolError{Code: CodePeerUnavailable, Message: "peer disconnected"}
	}
	if err := sendTo.send(outgoing); err != nil {
		if closeSession {
			return nil
		}
		return &ProtocolError{Code: CodePeerUnavailable, Message: "peer write failed"}
	}
	return nil
}

func errorMessage(code ErrorCode, detail string) *Message {
	return &Message{Type: TypeError, Code: code, Message: detail}
}

// Close stops accepting connections and releases every live session.
func (s *Server) Close() error {
	var err error
	s.closeOne.Do(func() {
		s.cancel()
		err = s.listener.Close()
		s.mu.Lock()
		for _, sess := range s.sessions {
			if sess.sender != nil {
				sess.sender.close()
			}
			if sess.relay != nil {
				sess.relay.close()
			}
		}
		s.sessions = make(map[string]*session)
		s.mu.Unlock()
		s.wg.Wait()
		close(s.closed)
	})
	return err
}
