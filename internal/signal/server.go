package signal

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
)

// Server routes signaling messages between clients of a session and drops
// duplicated or stale (out-of-order) sequenced messages.
type Server struct {
	ln net.Listener

	mu       sync.Mutex
	sessions map[string]map[string]*serverConn // session -> client id -> conn
	pending  map[string]map[string][]Message   // session -> target id -> queue
	lastSeq  map[string]map[string]uint64      // session -> sender id -> last seq
	conns    map[net.Conn]struct{}

	rejected atomic.Uint64
	closed   chan struct{}
	once     sync.Once
}

type serverConn struct {
	id  string
	wmu sync.Mutex
	w   io.Writer
}

// Listen starts a signaling server. The address must be a loopback IP.
func Listen(addr string) (*Server, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("signal: bad addr %q: %w", addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("signal: addr %q is not a loopback address", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{
		ln:       ln,
		sessions: map[string]map[string]*serverConn{},
		pending:  map[string]map[string][]Message{},
		lastSeq:  map[string]map[string]uint64{},
		conns:    map[net.Conn]struct{}{},
		closed:   make(chan struct{}),
	}
	return s, nil
}

// Addr returns the bound address.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Rejected returns the count of dropped duplicate/stale/undeliverable messages.
func (s *Server) Rejected() uint64 { return s.rejected.Load() }

// Close shuts the server down.
func (s *Server) Close() {
	s.once.Do(func() {
		close(s.closed)
		s.ln.Close()
		s.mu.Lock()
		for c := range s.conns {
			c.Close()
		}
		s.mu.Unlock()
	})
}

// Serve accepts connections until the server is closed or ctx is done.
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		select {
		case <-ctx.Done():
			s.Close()
		case <-s.closed:
		}
	}()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.closed:
				return nil
			default:
				return err
			}
		}
		s.mu.Lock()
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		go s.handle(c)
	}
}

func (s *Server) handle(c net.Conn) {
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		c.Close()
	}()
	r := bufio.NewReader(c)
	first, err := Decode(r)
	if err != nil || first.Kind != KindHello {
		return
	}
	sc := &serverConn{id: first.From, w: c}
	s.register(first.Session, sc)
	for _, m := range s.takePending(first.Session, sc.id) {
		s.route(m, false)
	}
	for {
		m, err := Decode(r)
		if err != nil {
			return
		}
		m.Session = first.Session
		m.From = sc.id
		s.route(m, true)
	}
}

func (s *Server) register(session string, sc *serverConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions[session] == nil {
		s.sessions[session] = map[string]*serverConn{}
	}
	s.sessions[session][sc.id] = sc
}

func (s *Server) takePending(session, id string) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	var msgs []Message
	if q := s.pending[session]; q != nil {
		msgs = q[id]
		delete(q, id)
	}
	return msgs
}

// route delivers m to its target. Sequence checking happens only on first
// arrival; messages flushed from the pending queue were already checked.
func (s *Server) route(m Message, checkSeq bool) {
	s.mu.Lock()
	if checkSeq && m.Kind.Sequenced() {
		if s.lastSeq[m.Session] == nil {
			s.lastSeq[m.Session] = map[string]uint64{}
		}
		if m.Seq <= s.lastSeq[m.Session][m.From] {
			s.mu.Unlock()
			s.reject(m, "stale or duplicate seq")
			return
		}
		s.lastSeq[m.Session][m.From] = m.Seq
	}
	target := s.sessions[m.Session][m.To]
	if target == nil && m.To != "" {
		q := s.pending[m.Session]
		if q == nil {
			q = map[string][]Message{}
			s.pending[m.Session] = q
		}
		if len(q[m.To]) < 64 {
			q[m.To] = append(q[m.To], m)
			s.mu.Unlock()
			return
		}
	}
	s.mu.Unlock()
	if target == nil {
		s.reject(m, "no such peer")
		return
	}
	target.wmu.Lock()
	_ = Encode(target.w, m)
	target.wmu.Unlock()
}

func (s *Server) reject(m Message, why string) {
	s.rejected.Add(1)
	s.mu.Lock()
	src := s.sessions[m.Session][m.From]
	s.mu.Unlock()
	if src == nil {
		return
	}
	src.wmu.Lock()
	_ = Encode(src.w, Message{Kind: KindError, Session: m.Session, To: m.From, Err: why})
	src.wmu.Unlock()
}
