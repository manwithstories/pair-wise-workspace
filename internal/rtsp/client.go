package rtsp

import (
	"fmt"
	"net"
	neturl "net/url"
	"strconv"
	"time"

	"github.com/bluenviron/gortsplib/v4/pkg/auth"
	"github.com/bluenviron/gortsplib/v4/pkg/base"
	"github.com/bluenviron/gortsplib/v4/pkg/conn"
	"github.com/bluenviron/gortsplib/v4/pkg/headers"
)

// Client drives one RTSP session through the explicit StateMachine and owns
// the control connection plus whichever Transport was selected.
type Client struct {
	ServerAddr string
	MediaURL   string
	Username   string
	Password   string
	SM         *StateMachine

	nc        net.Conn
	rc        *conn.Conn
	rtcp      *TCPTransport
	cseq      int
	sessionID string
	sender    *auth.Sender
	transport Transport
}

// Dial opens the TCP control connection. Only loopback targets are accepted;
// callers must enforce that before invoking Dial.
func (c *Client) Dial() error {
	nc, err := net.DialTimeout("tcp", c.ServerAddr, 3*time.Second)
	if err != nil {
		return &HandshakeError{Stage: "dial", Detail: err.Error()}
	}
	c.nc = nc
	c.rc = conn.NewConn(nc)
	return nil
}

// Transport returns the configured data-plane transport once SETUP completes.
func (c *Client) RTPTransport() Transport { return c.transport }

func (c *Client) nextCSeq() string {
	c.cseq++
	return strconv.Itoa(c.cseq)
}

func (c *Client) url() *base.URL {
	u, err := base.ParseURL(c.MediaURL)
	if err != nil {
		panic(err)
	}
	return u
}

// do sends a request to the session media URL, retrying once with digest
// credentials on a 401.
func (c *Client) do(method base.Method, target *State, hdr base.Header) (*base.Response, error) {
	return c.doTo(method, target, hdr, c.url())
}

// doTo is do() with an explicit request URL (used for resume query params).
func (c *Client) doTo(method base.Method, target *State, hdr base.Header, reqURL *base.URL) (*base.Response, error) {
	send := func() (*base.Response, error) {
		req := &base.Request{
			Method: method,
			URL:    reqURL,
			Header: base.Header{"Cseq": base.HeaderValue{c.nextCSeq()}},
		}
		for k, v := range hdr {
			req.Header[k] = v
		}
		if c.sessionID != "" {
			req.Header["Session"] = headers.Session{Session: c.sessionID}.Marshal()
		}
		if c.sender != nil {
			c.sender.AddAuthorization(req)
		}
		if err := c.rc.WriteRequest(req); err != nil {
			return nil, &HandshakeError{Stage: string(method), Detail: err.Error()}
		}
		res, err := c.rc.ReadResponse()
		if err != nil {
			return nil, &HandshakeError{Stage: string(method), Detail: err.Error()}
		}
		return res, nil
	}

	res, err := send()
	if err != nil {
		return nil, err
	}
	if res.StatusCode == base.StatusUnauthorized && c.sender == nil {
		www := res.Header["WWW-Authenticate"]
		sender, authErr := auth.NewSender(www, c.Username, c.Password)
		if authErr != nil {
			return nil, &AuthError{Stage: string(method)}
		}
		c.sender = sender
		res, err = send()
		if err != nil {
			return nil, err
		}
	}
	if res.StatusCode == base.StatusUnauthorized {
		return nil, &AuthError{Stage: string(method)}
	}
	if res.StatusCode != base.StatusOK {
		return nil, &HandshakeError{Stage: string(method), Status: int(res.StatusCode), Detail: res.StatusMessage}
	}
	if target != nil {
		if moveErr := c.SM.Move(*target); moveErr != nil {
			return nil, &HandshakeError{Stage: string(method), Detail: moveErr.Error()}
		}
	}
	if v := res.Header["Session"]; len(v) > 0 && c.sessionID == "" {
		var sh headers.Session
		if err := sh.Unmarshal(v); err == nil {
			c.sessionID = sh.Session
		}
	}
	return res, nil
}

// Handshake runs OPTIONS, DESCRIBE and SETUP, building the selected transport.
func (c *Client) Handshake(mode string, readTimeout time.Duration) error {
	if _, err := c.do(base.Options, &[]State{StateOptions}[0], nil); err != nil {
		return err
	}
	if _, err := c.do(base.Describe, &[]State{StateDescribe}[0], nil); err != nil {
		return err
	}

	hdr := base.Header{}
	switch mode {
	case "tcp":
		ids := [2]int{0, 1}
		delivery := headers.TransportDeliveryUnicast
		hdr["Transport"] = headers.Transport{
			Protocol:       headers.TransportProtocolTCP,
			Delivery:       &delivery,
			InterleavedIDs: &ids,
			Mode:           transportModePlay(),
		}.Marshal()
	default:
		ut, ports, err := NewUDPTransport(0, readTimeout)
		if err != nil {
			return &HandshakeError{Stage: "setup", Detail: err.Error()}
		}
		delivery := headers.TransportDeliveryUnicast
		hdr["Transport"] = headers.Transport{
			Protocol:    headers.TransportProtocolUDP,
			Delivery:    &delivery,
			ClientPorts: &ports,
			Mode:        transportModePlay(),
		}.Marshal()
		c.transport = ut
	}

	if _, err := c.do(base.Setup, &[]State{StateSetup}[0], hdr); err != nil {
		return err
	}
	if mode == "tcp" {
		// Reuse the single buffered RTSP conn so no bytes get stranded between
		// two bufio.Readers when interleaved data starts flowing.
		c.rtcp = NewTCPTransportFromConn(c.nc, c.rc, readTimeout)
		c.transport = c.rtcp
	}
	return nil
}

// Play issues PLAY with an optional resume sequence (0 for a fresh session)
// and advances the machine into the Play state.
// Play issues PLAY with an optional resume position (logical packet position,
// 0 for a fresh session). The base MediaURL is never mutated: the resume query
// is applied to a per-request URL so reconnects rebuild it cleanly.
func (c *Client) Play(resumeSeq int) error {
	target := c.url()
	if resumeSeq > 0 {
		u := (*neturl.URL)(target)
		q := u.Query()
		q.Set("resume", strconv.Itoa(resumeSeq))
		u.RawQuery = q.Encode()
		target = (*base.URL)(u)
	}
	_, err := c.doTo(base.Play, &[]State{StatePlay}[0], nil, target)
	return err
}

// Pause moves the session to Pause, used before reconnect/teardown.
func (c *Client) Pause() error {
	_, err := c.do(base.Pause, &[]State{StatePause}[0], nil)
	return err
}

// Teardown ends the session and always closes the control connection.
func (c *Client) Teardown() error {
	_, err := c.do(base.Teardown, &[]State{StateTeardown}[0], nil)
	if c.nc != nil {
		_ = c.nc.Close()
	}
	return err
}

// Close releases the control connection and transport sockets.
func (c *Client) Close() {
	if c.transport != nil {
		_ = c.transport.Close()
	}
	if c.nc != nil {
		_ = c.nc.Close()
	}
}

func transportModePlay() *headers.TransportMode {
	m := headers.TransportModePlay
	return &m
}

// String helper for readable error context.
func (c *Client) String() string {
	return fmt.Sprintf("rtsp.Client{addr=%s transport=%v}", c.ServerAddr, c.transport != nil)
}
