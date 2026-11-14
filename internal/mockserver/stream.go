package mockserver

import (
	"io"
	"net"
	netURL "net/url"
	"strconv"

	"github.com/bluenviron/gortsplib/v4/pkg/base"
	"github.com/bluenviron/gortsplib/v4/pkg/conn"
	"github.com/example/rtpcap/internal/scenario"
)

func (s *Server) stream(sess *session, rc *conn.Conn, resumeAt int) error {
	plan := s.sc.BuildSendPlan()
	cut := s.sc.EventOf(scenario.EventCut)
	var udpTarget *net.UDPAddr
	if sess.transport == "udp" {
		udpTarget = &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: sess.clientPorts[0]}
	}

	for _, slot := range plan {
		if slot.Pos < resumeAt {
			continue
		}
		// The cut already happened before this (resume) connection; only the
		// initial connection is torn down, so a resumed stream runs to the end.
		if cut != nil && resumeAt == 0 && slot.Pos > cut.Pos {
			_ = sess.conn.Close()
			return io.EOF
		}
		if sess.transport == "tcp" {
			fr := base.InterleavedFrame{Channel: rtpChannel, Payload: slot.Raw}
			buf := make([]byte, fr.MarshalSize())
			if _, err := fr.MarshalTo(buf); err != nil {
				return err
			}
			if _, err := sess.conn.Write(buf); err != nil {
				return err
			}
		} else {
			if err := s.writeUDP(sess, udpTarget, slot.Raw); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Server) writeUDP(sess *session, target *net.UDPAddr, raw []byte) error {
	if sess.udpRTP == nil {
		conn, err := net.DialUDP("udp", nil, target)
		if err != nil {
			return err
		}
		sess.udpRTP = conn
	}
	_, err := sess.udpRTP.Write(raw)
	return err
}

func resumeFromURL(u *base.URL) int {
	if u == nil {
		return 0
	}
	for k, vs := range (*netURL.URL)(u).Query() {
		if k == "resume" && len(vs) > 0 {
			if n, err := strconv.Atoi(vs[0]); err == nil {
				return n
			}
		}
	}
	return 0
}
