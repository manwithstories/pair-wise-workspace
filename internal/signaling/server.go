package signaling

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults for the loopback signaling server. They are short on purpose: the
// whole exchange happens between two processes on the same machine.
const (
	defaultIdleTimeout  = 5 * time.Second
	defaultMaxLineSize  = 1 << 20
	stateOfferAccepted  = "offer_accepted"
	stateJoin           = "join"
	stateAnswerAccepted = "answer_accepted"
	statePeerClosed     = "peer_closed"
	stateSessionExpired = "session_expired"
)

// Rejection records why a message was refused. Requirement 3 asks for the
// reason behind every loopback violation to be recorded, not just counted.
type Rejection struct {
	At      time.Time `json:"at"`
	Code    string    `json:"code"`
	Session string    `json:"session,omitempty"`
	Detail  string    `json:"detail"`
}

// Stats is a snapshot of server activity. It is cheap and safe to read while
// the server is running.
type Stats struct {
	ConnectionsAccepted int64            `json:"connections_accepted"`
	ConnectionsClosed   int64            `json:"connections_closed"`
	MessagesIn          int64            `json:"messages_in"`
	MessagesForwarded   int64            `json:"messages_forwarded"`
	MessagesRejected    int64            `json:"messages_rejected"`
	SessionsCreated     int64            `json:"sessions_created"`
	SessionsExpired     int64            `json:"sessions_expired"`
	RejectedByCode      map[string]int64 `json:"rejected_by_code"`
	Rejections          []Rejection      `json:"rejections"`
}

// Server is a self-contained TCP signaling broker for two peers. It owns no
// peer state beyond the SDP and trickled candidates, so it can be embedded in
// a test and torn down without leaving goroutines or sockets behind.
type Server struct {
	ln       net.Listener
	idle     time.Duration
	maxLine  int
	mu       sync.Mutex
	sessions map[string]*session
	closed   bool
	wg       sync.WaitGroup
	done     chan struct{}
	now      func() time.Time

	connectionsAccepted atomic.Int64
	connectionsClosed   atomic.Int64
	messagesIn          atomic.Int64
	messagesForwarded   atomic.Int64
	messagesRejected    atomic.Int64
	sessionsCreated     atomic.Int64
	sessionsExpired     atomic.Int64

	statsMu  sync.Mutex
	byCode   map[string]int64
	rejected []Rejection
}

type session struct {
	id       string
	offer    Message
	offerTo  *serverConn
	answerTo *serverConn
	lastSeq  map[*serverConn]uint64
	created  time.Time
	lastSeen time.Time
	pending  map[*serverConn][]Message
}

// Listen starts a signaling server on addr (use "127.0.0.1:0" for a free
// port). The listener is already accepting when it returns.
func Listen(addr string, idle time.Duration) (*Server, error) {
	if idle <= 0 {
		idle = defaultIdleTimeout
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	s := &Server{
		ln:       ln,
		idle:     idle,
		maxLine:  defaultMaxLineSize,
		sessions: make(map[string]*session),
		done:     make(chan struct{}),
		now:      time.Now,
		byCode:   make(map[string]int64),
	}
	s.wg.Add(2)
	go s.acceptLoop()
	go s.reapLoop()
	return s, nil
}

// Addr returns the bound TCP address, useful when listening on port 0.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Close stops accepting, closes every session and waits for all server
// goroutines to exit. It is safe to call more than once.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.done)
	var conns []*serverConn
	for _, sess := range s.sessions {
		if sess.offerTo != nil {
			conns = append(conns, sess.offerTo)
		}
		if sess.answerTo != nil {
			conns = append(conns, sess.answerTo)
		}
	}
	s.sessions = make(map[string]*session)
	s.mu.Unlock()

	for _, c := range conns {
		c.close()
	}
	err := s.ln.Close()
	s.wg.Wait()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// Stats returns a copy of the counters plus the most recent rejections.
func (s *Server) Stats() Stats {
	s.statsMu.Lock()
	byCode := make(map[string]int64, len(s.byCode))
	for k, v := range s.byCode {
		byCode[k] = v
	}
	rejected := make([]Rejection, len(s.rejected))
	copy(rejected, s.rejected)
	s.statsMu.Unlock()

	return Stats{
		ConnectionsAccepted: s.connectionsAccepted.Load(),
		ConnectionsClosed:   s.connectionsClosed.Load(),
		MessagesIn:          s.messagesIn.Load(),
		MessagesForwarded:   s.messagesForwarded.Load(),
		MessagesRejected:    s.messagesRejected.Load(),
		SessionsCreated:     s.sessionsCreated.Load(),
		SessionsExpired:     s.sessionsExpired.Load(),
		RejectedByCode:      byCode,
		Rejections:          rejected,
	}
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.connectionsAccepted.Add(1)
		sc := &serverConn{conn: conn, server: s, id: conn.RemoteAddr().String()}
		s.wg.Add(1)
		go s.serveConn(sc)
	}
}

func (s *Server) reapLoop() {
	defer s.wg.Done()
	interval := s.idle / 4
	if interval < 20*time.Millisecond {
		interval = 20 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.expireIdle()
		}
	}
}

func (s *Server) expireIdle() {
	var expired []*session
	s.mu.Lock()
	now := s.now()
	for id, sess := range s.sessions {
		if now.Sub(sess.lastSeen) >= s.idle {
			expired = append(expired, sess)
			delete(s.sessions, id)
		}
	}
	s.mu.Unlock()

	for _, sess := range expired {
		s.sessionsExpired.Add(1)
		msg := ErrorMessage(0, sess.id, ErrCodeTimeout, fmt.Sprintf("session idle for %s", s.idle))
		for _, sc := range []*serverConn{sess.offerTo, sess.answerTo} {
			if sc != nil {
				_ = sc.write(msg)
				sc.close()
			}
		}
	}
}

func (s *Server) serveConn(sc *serverConn) {
	defer s.wg.Done()
	defer func() {
		sc.close()
		s.connectionsClosed.Add(1)
		s.dropConn(sc)
	}()

	reader := bufio.NewReaderSize(sc.conn, 64*1024)
	for {
		line, err := readLine(reader, s.maxLine)
		if err != nil {
			if errors.Is(err, errLineTooLong) {
				s.reject(sc, "", newProtocolError(ErrCodeInvalidJSON, "line exceeds %d bytes", s.maxLine))
			}
			return
		}
		s.messagesIn.Add(1)
		msg, err := Decode(line)
		if err != nil {
			s.rejectErr(sc, msg, err)
			continue
		}
		if err := s.handle(sc, msg); err != nil {
			s.rejectErr(sc, msg, err)
		}
	}
}

var errLineTooLong = errors.New("line too long")

func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			return nil, err
		}
		if len(buf)+len(chunk) > max {
			return nil, errLineTooLong
		}
		buf = append(buf, chunk...)
		if !isPrefix {
			return buf, nil
		}
	}
}

func (s *Server) rejectErr(sc *serverConn, msg Message, err error) {
	code := ErrCode(err)
	if code == "" {
		code = ErrCodeInternal
	}
	s.reject(sc, msg.Session, &ProtocolError{Code: code, Detail: err.Error()})
}

func (s *Server) reject(sc *serverConn, sessionID string, perr *ProtocolError) {
	s.messagesRejected.Add(1)
	s.statsMu.Lock()
	s.byCode[perr.Code]++
	rec := Rejection{At: s.now(), Code: perr.Code, Session: sessionID, Detail: perr.Detail}
	s.rejected = append(s.rejected, rec)
	if len(s.rejected) > 32 {
		s.rejected = s.rejected[len(s.rejected)-32:]
	}
	s.statsMu.Unlock()
	_ = sc.write(ErrorMessage(0, sessionID, perr.Code, perr.Detail))
}
