// Package mocksrv is a loopback-only RTSP server used by the selftest and
// by cmd/mockrtsp. It speaks enough RTSP to exercise the client state
// machine (OPTIONS, DESCRIBE with digest auth, SETUP, PLAY, TEARDOWN) and
// streams synthetic H.264/RTP over UDP or TCP-interleaved with
// deterministic fault injection.
package mocksrv

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/bluenviron/gortsplib/v4/pkg/base"
)

// Config controls the generated stream and injected faults.
type Config struct {
	Addr            string // listen address, loopback only
	Path            string // stream path, e.g. /live
	Tracks          int    // number of SDP tracks (1..2)
	Packets         int    // total RTP packets across all tracks
	PacketsPerFrame int    // RTP packets per access unit
	Rate            int    // send pace in packets/second
	Drop            int    // packets to drop per track
	Dup             int    // packets to duplicate per track
	Reorder         int    // adjacent packet swaps per track
	Seed            int64  // deterministic payload/fault seed
	User            string // digest username, empty disables auth
	Pass            string
}

func (c *Config) normalize() {
	if c.Addr == "" {
		c.Addr = "127.0.0.1:8554"
	}
	if c.Path == "" {
		c.Path = "/live"
	}
	if c.Tracks == 0 {
		c.Tracks = 2
	}
	if c.Packets == 0 {
		c.Packets = 500
	}
	if c.PacketsPerFrame <= 0 {
		c.PacketsPerFrame = 4
	}
	if c.Rate <= 0 {
		c.Rate = 500
	}
}

// Server is the mock RTSP server. It only ever binds 127.0.0.1.
type Server struct {
	cfg   Config
	nonce string

	ln     net.Listener
	sessN  atomic.Int64
	closed atomic.Bool
	wg     sync.WaitGroup

	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

// New builds a server; Start must be called to bind and accept.
func New(cfg Config) *Server {
	cfg.normalize()
	return &Server{cfg: cfg, nonce: fmt.Sprintf("%016x", cfg.Seed), conns: make(map[net.Conn]struct{})}
}

// Start binds the listener and launches the accept loop.
func (s *Server) Start() error {
	host, _, err := net.SplitHostPort(s.cfg.Addr)
	if err != nil {
		return err
	}
	if host != "127.0.0.1" && host != "localhost" {
		return fmt.Errorf("mocksrv: refusing to bind non-loopback %q", host)
	}
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("mocksrv: listen: %w", err)
	}
	s.ln = ln
	s.wg.Add(1)
	go s.acceptLoop()
	return nil
}

// Addr returns the bound address.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Close stops accepting, closes live connections and waits for handlers.
func (s *Server) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	_ = s.ln.Close()
	s.mu.Lock()
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
	return nil
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns[nc] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.conns, nc)
				s.mu.Unlock()
			}()
			s.handleConn(nc)
		}()
	}
}

// trackSetup records how one track was negotiated.
type trackSetup struct {
	interleaved bool
	channel     int
	udpDst      *net.UDPAddr
	udpConn     *net.UDPConn
}

// connState is the per-connection RTSP session.
type connState struct {
	id      string
	tracks  map[int]*trackSetup
	stream  *streamer
	writeMu sync.Mutex
}

func (s *Server) handleConn(nc net.Conn) {
	defer nc.Close()
	st := &connState{tracks: make(map[int]*trackSetup)}
	br := bufio.NewReader(nc)
	write := func(res *base.Response) {
		st.writeMu.Lock()
		defer st.writeMu.Unlock()
		if raw, err := res.Marshal(); err == nil {
			_, _ = nc.Write(raw)
		}
	}
	for {
		req := &base.Request{}
		if err := req.Unmarshal(br); err != nil {
			s.stopStream(st)
			return
		}
		switch req.Method {
		case base.Options:
			write(ok(req, base.Header{
				"Public": base.HeaderValue{"OPTIONS, DESCRIBE, SETUP, PLAY, TEARDOWN"},
			}))
		case base.Describe:
			s.handleDescribe(req, write)
		case base.Setup:
			if !s.handleSetup(nc, req, st, write) {
				return
			}
		case base.Play:
			s.handlePlay(nc, req, st, write)
		case base.Teardown:
			s.stopStream(st)
			write(ok(req, base.Header{"Session": base.HeaderValue{st.id}}))
		default:
			write(&base.Response{StatusCode: base.StatusBadRequest, Header: cseqOf(req)})
		}
	}
}

func (s *Server) stopStream(st *connState) {
	if st.stream != nil {
		st.stream.stop()
		st.stream = nil
	}
}

func ok(req *base.Request, h base.Header) *base.Response {
	if h == nil {
		h = make(base.Header)
	}
	for k, v := range cseqOf(req) {
		h[k] = v
	}
	return &base.Response{StatusCode: base.StatusOK, Header: h}
}

func cseqOf(req *base.Request) base.Header {
	if v := req.Header["CSeq"]; len(v) > 0 {
		return base.Header{"CSeq": v}
	}
	return base.Header{}
}

// ParseFaults parses a "drop=N,dup=N,reorder=N" fault specification.
func ParseFaults(s string) (drop, dup, reorder int, err error) {
	if s == "" {
		return 0, 0, 0, nil
	}
	for _, part := range strings.Split(s, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			return 0, 0, 0, fmt.Errorf("mocksrv: bad fault spec %q", part)
		}
		n, cerr := strconv.Atoi(kv[1])
		if cerr != nil || n < 0 {
			return 0, 0, 0, fmt.Errorf("mocksrv: bad fault count %q", kv[1])
		}
		switch kv[0] {
		case "drop":
			drop = n
		case "dup":
			dup = n
		case "reorder":
			reorder = n
		default:
			return 0, 0, 0, fmt.Errorf("mocksrv: unknown fault %q", kv[0])
		}
	}
	return drop, dup, reorder, nil
}

// parseTransportHeader splits a Transport header value into lowercased
// key=value pairs and bare tokens.
func parseTransportHeader(v string) map[string]string {
	out := make(map[string]string)
	for _, part := range strings.Split(v, ";") {
		part = strings.TrimSpace(part)
		if i := strings.IndexByte(part, '='); i >= 0 {
			out[strings.ToLower(part[:i])] = part[i+1:]
		} else {
			out[strings.ToLower(part)] = ""
		}
	}
	return out
}
