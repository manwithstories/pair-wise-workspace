package mockserver

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

type request struct {
	method  string
	url     string
	cseq    string
	headers map[string]string
	body    []byte
}

func readRequest(r *bufio.Reader) (request, error) {
	var req request
	line, err := r.ReadString('\n')
	if err != nil {
		return req, err
	}
	parts := strings.Fields(strings.TrimSpace(line))
	if len(parts) != 3 || parts[2] != "RTSP/1.0" {
		return req, fmt.Errorf("invalid RTSP request line %q", line)
	}
	req.method, req.url, req.headers = parts[0], parts[1], make(map[string]string)
	for {
		line, err = r.ReadString('\n')
		if err != nil {
			return req, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return req, fmt.Errorf("invalid RTSP header %q", line)
		}
		req.headers[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	req.cseq = req.headers["cseq"]
	if req.cseq == "" {
		return req, fmt.Errorf("missing CSeq")
	}
	if text := req.headers["content-length"]; text != "" {
		n, err := strconv.Atoi(text)
		if err != nil || n < 0 || n > 1<<20 {
			return req, fmt.Errorf("invalid Content-Length %q", text)
		}
		req.body = make([]byte, n)
		if _, err := io.ReadFull(r, req.body); err != nil {
			return req, err
		}
	}
	return req, nil
}

type session struct {
	server         *Server
	conn           net.Conn
	reader         *bufio.Reader
	writeMu        sync.Mutex
	id             string
	transportReady bool
	tcpMode        bool
	clientRTP      *net.UDPAddr
	clientRTCP     *net.UDPAddr
	rtpConn        *net.UDPConn
	rtcpConn       *net.UDPConn
	lastActivity   time.Time
	seenOPTIONS    bool
	seenDESCRIBE   bool
	streamMu       sync.Mutex
	cancel         context.CancelFunc
	nextPacket     int
	stalled        bool
	complete       bool
}

func (s *Server) serve(conn net.Conn) {
	sess := &session{server: s, conn: conn, reader: bufio.NewReader(conn),
		id: fmt.Sprintf("audit-%d", s.nextID.Add(1)), lastActivity: time.Now()}
	defer sess.close()
	for {
		req, err := readRequest(sess.reader)
		if err != nil {
			return
		}
		if !sess.handle(req) {
			return
		}
	}
}

func (s *session) handle(req request) bool {
	if s.server.cfg.SessionTimeout > 0 && req.headers["session"] != "" &&
		time.Since(s.lastActivity) > s.server.cfg.SessionTimeout {
		s.writeResponse(req.cseq, "454 Session Not Found", nil, nil)
		return false
	}
	s.lastActivity = time.Now()
	switch req.method {
	case "OPTIONS":
		s.seenOPTIONS = true
		s.writeResponse(req.cseq, "200 OK", map[string]string{
			"Public": "OPTIONS, DESCRIBE, SETUP, PLAY, PAUSE, TEARDOWN",
		}, nil)
	case "DESCRIBE":
		if s.server.cfg.Policy == "strict" && !s.seenOPTIONS {
			s.writeResponse(req.cseq, "455 Method Not Valid in This State", nil, nil)
			return true
		}
		s.seenDESCRIBE = true
		s.writeResponse(req.cseq, "200 OK", map[string]string{
			"Content-Type": "application/sdp", "Content-Base": s.server.URL() + "/",
		}, s.server.sdp)
	case "SETUP":
		if !s.handleSetup(req) {
			return true
		}
	case "PLAY":
		return s.play(req)
	case "PAUSE":
		s.cancelStream()
		s.writeResponse(req.cseq, "200 OK", s.sessionHeaders(), nil)
	case "TEARDOWN":
		s.cancelStream()
		s.writeResponse(req.cseq, "200 OK", s.sessionHeaders(), nil)
		return false
	default:
		s.writeResponse(req.cseq, "501 Not Implemented", nil, nil)
	}
	return true
}

func (s *session) sessionHeaders() map[string]string {
	headers := map[string]string{"Session": s.id}
	if s.server.cfg.SessionTimeout > 0 {
		seconds := int(s.server.cfg.SessionTimeout / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		headers["Session"] += ";timeout=" + strconv.Itoa(seconds)
	}
	return headers
}

func (s *session) writeResponse(cseq, status string, headers map[string]string, body []byte) error {
	var b strings.Builder
	fmt.Fprintf(&b, "RTSP/1.0 %s\r\nCSeq: %s\r\nServer: rtspaudit-mock\r\n", status, cseq)
	for key, value := range headers {
		fmt.Fprintf(&b, "%s: %s\r\n", key, value)
	}
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n", len(body))
	b.Write(body)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := io.WriteString(s.conn, b.String())
	return err
}

func (s *session) close() {
	s.cancelStream()
	if s.rtpConn != nil {
		s.rtpConn.Close()
	}
	if s.rtcpConn != nil {
		s.rtcpConn.Close()
	}
}

func (s *session) cancelStream() {
	s.streamMu.Lock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.streamMu.Unlock()
}
