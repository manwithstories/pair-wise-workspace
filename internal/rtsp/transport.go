// Package rtsp implements the two RTP transports (UDP unicast and TCP
// interleaved) behind one abstraction. Both share the control connection
// with its single CSeq counter and deliver parsed RTP packets through the
// same callback, so the session FSM on top is transport-agnostic.
package rtsp

import (
	"bufio"
	"fmt"
	"net"
	"net/url"
	"strconv"

	"github.com/bluenviron/gortsplib/v4/pkg/base"
	"github.com/pion/rtp"

	"rtspreplay/internal/sdp"
)

// PacketHandler receives one parsed RTP packet and the index of the track
// it belongs to.
type PacketHandler func(trackIdx int, pkt *rtp.Packet)

// ErrorHandler reports a fatal media-channel error, e.g. the server
// closing the connection mid-stream.
type ErrorHandler func(err error)

// Transport is the control+media channel of one RTSP session. The session
// FSM drives any implementation without knowing which one is underneath.
type Transport interface {
	// Connect dials the control connection.
	Connect() error
	// Do sends one request with the next CSeq and reads its response.
	Do(req *base.Request) (*base.Response, error)
	// SetupTrack negotiates one track and returns the session ID the
	// server assigned in the SETUP response.
	SetupTrack(track *sdp.Track, sessionID string) (string, error)
	// StartRTP begins media delivery; callbacks fire from reader
	// goroutines until Close or a fatal error.
	StartRTP(onPkt PacketHandler, onErr ErrorHandler) error
	// Teardown sends a best-effort TEARDOWN request.
	Teardown(sessionID string)
	// Close releases every socket, including RTP/RTCP ports.
	Close() error
}

// controlConn is the shared RTSP control connection: one TCP connection,
// one monotonically increasing CSeq.
type controlConn struct {
	nc   net.Conn
	br   *bufio.Reader
	cseq int
}

func dialControl(addr string) (*controlConn, error) {
	nc, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("rtsp: dial %s: %w", addr, err)
	}
	return &controlConn{nc: nc, br: bufio.NewReader(nc)}, nil
}

// do writes the request with a fresh CSeq and blocks for the response.
func (c *controlConn) do(req *base.Request) (*base.Response, error) {
	c.cseq++
	if req.Header == nil {
		req.Header = make(base.Header)
	}
	req.Header["CSeq"] = base.HeaderValue{strconv.Itoa(c.cseq)}
	raw, err := req.Marshal()
	if err != nil {
		return nil, fmt.Errorf("rtsp: marshal request: %w", err)
	}
	if _, err := c.nc.Write(raw); err != nil {
		return nil, fmt.Errorf("rtsp: write request: %w", err)
	}
	res := &base.Response{}
	if err := res.Unmarshal(c.br); err != nil {
		return nil, fmt.Errorf("rtsp: read response: %w", err)
	}
	return res, nil
}

// writeFireAndForget sends a request without reading the response. Used
// for TEARDOWN on the interleaved transport, where pending $ frames make a
// synchronous response read unreliable.
func (c *controlConn) writeFireAndForget(req *base.Request) {
	c.cseq++
	if req.Header == nil {
		req.Header = make(base.Header)
	}
	req.Header["CSeq"] = base.HeaderValue{strconv.Itoa(c.cseq)}
	if raw, err := req.Marshal(); err == nil {
		_, _ = c.nc.Write(raw)
	}
}

func (c *controlConn) close() {
	if c.nc != nil {
		_ = c.nc.Close()
	}
}

// mustParseURL converts a string into a base.URL. All URLs passed here are
// built by this package or come from the server's SDP; a parse failure
// yields nil and surfaces as a marshal error in Do.
func mustParseURL(s string) *base.URL {
	u, err := url.Parse(s)
	if err != nil {
		return nil
	}
	bu := base.URL(*u)
	return &bu
}

// parseSessionID extracts the session ID from a SETUP response header,
// stripping any ";timeout=" suffix.
func parseSessionID(res *base.Response) string {
	v := res.Header["Session"]
	if len(v) == 0 {
		return ""
	}
	id := v[0]
	for i := 0; i < len(id); i++ {
		if id[i] == ';' {
			return id[:i]
		}
	}
	return id
}
