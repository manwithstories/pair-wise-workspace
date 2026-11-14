// Package mockserver is a scripted, loopback-only RTSP server used by the
// capture command. It serves one deterministic RTP stream over either TCP
// interleaved frames or dual-port UDP and can inject loss, reordering,
// duplicates, late packets, mid-session cuts and persistent 401 failures.
package mockserver

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"

	"github.com/bluenviron/gortsplib/v4/pkg/auth"
	"github.com/bluenviron/gortsplib/v4/pkg/base"
	"github.com/bluenviron/gortsplib/v4/pkg/conn"
	"github.com/bluenviron/gortsplib/v4/pkg/headers"
	"github.com/example/rtpcap/internal/scenario"
)

const (
	realm       = "rtpcap-loopback"
	rtpChannel  = 0
	rtcpChannel = 1
)

// Server is the scripted RTSP server.
type Server struct {
	sc       *scenario.Scenario
	listener net.Listener
	nonce    string
	logger   *slog.Logger

	mu       sync.Mutex
	closed   bool
	sessions map[string]*session
}

type session struct {
	id          string
	transport   string // "tcp" | "udp"
	clientPorts [2]int
	conn        net.Conn
	udpRTP      *net.UDPConn
}

// New binds the RTSP listener on 127.0.0.1 with an ephemeral port.
func New(sc *scenario.Scenario, logger *slog.Logger) (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("mock server listen: %w", err)
	}
	nonceBytes := make([]byte, 8)
	_, _ = rand.Read(nonceBytes)
	return &Server{
		sc:       sc,
		listener: ln,
		nonce:    hex.EncodeToString(nonceBytes),
		logger:   logger,
		sessions: map[string]*session{},
	}, nil
}

// Addr returns the loopback RTSP address.
func (s *Server) Addr() string {
	return s.listener.Addr().String()
}

// Start accepts connections until Close.
func (s *Server) Start() {
	go func() {
		for {
			nc, err := s.listener.Accept()
			if err != nil {
				return
			}
			go s.handle(nc)
		}
	}()
}

// Close stops listening and drops every session.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	sessions := s.sessions
	s.sessions = map[string]*session{}
	s.mu.Unlock()

	_ = s.listener.Close()
	for _, sess := range sessions {
		_ = sess.conn.Close()
		if sess.udpRTP != nil {
			_ = sess.udpRTP.Close()
		}
	}
}

func (s *Server) handle(nc net.Conn) {
	defer nc.Close()
	rc := conn.NewConn(nc)
	sess := &session{id: newSessionID(), conn: nc}

	for {
		what, err := rc.Read()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
				s.logger.Debug("rtsp read ended", "err", err)
			}
			return
		}
		req, ok := what.(*base.Request)
		if !ok {
			continue
		}
		if err := s.dispatch(rc, req, sess); err != nil {
			s.logger.Debug("rtsp dispatch ended", "method", req.Method, "err", err)
			return
		}
	}
}

func (s *Server) dispatch(rc *conn.Conn, req *base.Request, sess *session) error {
	switch req.Method {
	case base.Options:
		return s.reply(rc, req, base.StatusOK, base.Header{}, nil)
	case base.Describe:
		return s.handleDescribe(rc, req)
	case base.Setup:
		return s.handleSetup(rc, req, sess)
	case base.Play:
		return s.handlePlay(rc, req, sess)
	case base.Pause:
		return s.reply(rc, req, base.StatusOK, base.Header{}, nil)
	case base.Teardown:
		_ = s.reply(rc, req, base.StatusOK, base.Header{}, nil)
		return io.EOF
	default:
		return s.reply(rc, req, base.StatusMethodNotAllowed, base.Header{}, nil)
	}
}

func (s *Server) handleDescribe(rc *conn.Conn, req *base.Request) error {
	if s.sc.AuthEnabled {
		if err := auth.Validate(req, s.sc.Username, s.sc.Password,
			[]auth.ValidateMethod{auth.ValidateMethodDigestMD5}, realm, s.nonce); err != nil {
			hdr := base.Header{}
			hdr["WWW-Authenticate"] = auth.GenerateWWWAuthenticate(
				[]auth.ValidateMethod{auth.ValidateMethodDigestMD5}, realm, s.nonce)
			return s.reply(rc, req, base.StatusUnauthorized, hdr, nil)
		}
	}
	sdp := []byte("v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=rtpcap\r\n" +
		"t=0 0\r\nm=video 0 RTP/AVP 96\r\na=rtpmap:96 raw/90000\r\n" +
		"a=control:stream0\r\n")
	hdr := base.Header{"Content-Type": base.HeaderValue{"application/sdp"}}
	return s.reply(rc, req, base.StatusOK, hdr, sdp)
}

func (s *Server) handleSetup(rc *conn.Conn, req *base.Request, sess *session) error {
	if s.sc.AuthEnabled {
		if err := auth.Validate(req, s.sc.Username, s.sc.Password,
			[]auth.ValidateMethod{auth.ValidateMethodDigestMD5}, realm, s.nonce); err != nil {
			hdr := base.Header{}
			hdr["WWW-Authenticate"] = auth.GenerateWWWAuthenticate(
				[]auth.ValidateMethod{auth.ValidateMethodDigestMD5}, realm, s.nonce)
			return s.reply(rc, req, base.StatusUnauthorized, hdr, nil)
		}
	}

	var th headers.Transport
	if err := th.Unmarshal(req.Header["Transport"]); err != nil {
		return s.reply(rc, req, base.StatusBadRequest, base.Header{}, nil)
	}
	hdr := base.Header{}
	respTransport := headers.Transport{
		Protocol:       th.Protocol,
		Mode:           th.Mode,
		InterleavedIDs: &[2]int{rtpChannel, rtcpChannel},
	}
	if th.Delivery != nil && *th.Delivery == headers.TransportDeliveryUnicast &&
		th.Protocol == headers.TransportProtocolUDP && th.ClientPorts != nil {
		sess.transport = "udp"
		sess.clientPorts = *th.ClientPorts
		respTransport.Delivery = th.Delivery
		respTransport.ClientPorts = th.ClientPorts
		serverPorts := [2]int{0, 0}
		respTransport.ServerPorts = &serverPorts
	} else {
		sess.transport = "tcp"
	}
	hdr["Transport"] = respTransport.Marshal()
	hdr["Session"] = headers.Session{Session: sess.id, Timeout: uintPtr(30)}.Marshal()
	s.mu.Lock()
	s.sessions[sess.id] = sess
	s.mu.Unlock()
	return s.reply(rc, req, base.StatusOK, hdr, nil)
}

func (s *Server) handlePlay(rc *conn.Conn, req *base.Request, sess *session) error {
	if s.sc.AuthEnabled {
		if err := auth.Validate(req, s.sc.Username, s.sc.Password,
			[]auth.ValidateMethod{auth.ValidateMethodDigestMD5}, realm, s.nonce); err != nil {
			hdr := base.Header{}
			hdr["WWW-Authenticate"] = auth.GenerateWWWAuthenticate(
				[]auth.ValidateMethod{auth.ValidateMethodDigestMD5}, realm, s.nonce)
			return s.reply(rc, req, base.StatusUnauthorized, hdr, nil)
		}
	}
	if s.sc.EventOf(scenario.EventAuthFail) != nil {
		hdr := base.Header{}
		hdr["WWW-Authenticate"] = auth.GenerateWWWAuthenticate(
			[]auth.ValidateMethod{auth.ValidateMethodDigestMD5}, realm, s.nonce)
		return s.reply(rc, req, base.StatusUnauthorized, hdr, nil)
	}
	hdr := base.Header{"Session": headers.Session{Session: sess.id}.Marshal()}
	if err := s.reply(rc, req, base.StatusOK, hdr, nil); err != nil {
		return err
	}
	resumeAt := resumeFromURL(req.URL)
	return s.stream(sess, rc, resumeAt)
}

func (s *Server) reply(rc *conn.Conn, req *base.Request, status base.StatusCode,
	hdr base.Header, body []byte) error {
	if hdr == nil {
		hdr = base.Header{}
	}
	if body != nil {
		hdr["Content-Length"] = base.HeaderValue{strconv.Itoa(len(body))}
	}
	hdr["Cseq"] = req.Header["Cseq"]
	res := &base.Response{
		StatusCode: status,
		Header:     hdr,
		Body:       body,
	}
	return rc.WriteResponse(res)
}

func uintPtr(v uint) *uint { return &v }

func newSessionID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
