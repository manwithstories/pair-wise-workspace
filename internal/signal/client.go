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

// ClientOptions tune reconnect and heartbeat behavior.
type ClientOptions struct {
	ReconnectWindow   time.Duration
	HeartbeatInterval time.Duration
	WriteTimeout      time.Duration
	MessageBuffer     int
}

func (o ClientOptions) withDefaults() ClientOptions {
	if o.ReconnectWindow <= 0 {
		o.ReconnectWindow = 3 * time.Second
	}
	if o.HeartbeatInterval <= 0 {
		o.HeartbeatInterval = 5 * time.Second
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = 3 * time.Second
	}
	if o.MessageBuffer <= 0 {
		o.MessageBuffer = 128
	}
	return o
}

// ErrSignalTimeout indicates that the initial connection or reconnect window
// elapsed without a usable TCP stream.
var ErrSignalTimeout = errors.New("signal: timeout")

// Client is a signalling participant. It owns exactly one logical role in a
// session and keeps the TCP stream alive with heartbeats.
type Client struct {
	addr      string
	sessionID string
	role      Role
	options   ClientOptions
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOne  sync.Once

	connMu  sync.Mutex
	conn    net.Conn
	writeMu sync.Mutex

	msgs   chan *Message
	errs   chan error
	closed atomic.Bool
}

// Dial connects a role to a loopback signalling server. It waits for the first
// TCP connection (up to ReconnectWindow) and then maintains it in background.
func Dial(parent context.Context, addr, sessionID string, role Role, options ClientOptions) (*Client, error) {
	if role != RoleSender && role != RoleRelay {
		return nil, fmt.Errorf("signal: invalid role %q", role)
	}
	probe := &Message{Type: TypeHello, SessionID: sessionID, Role: role}
	if err := Validate(probe); err != nil {
		return nil, err
	}
	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("resolve signal server: %w", err)
	}
	if tcpAddr.IP == nil || !tcpAddr.IP.IsLoopback() {
		return nil, fmt.Errorf("%w: signal server %s", ErrNonLoopbackCandidate, addr)
	}

	ctx, cancel := context.WithCancel(parent)
	options = options.withDefaults()
	c := &Client{
		addr:      addr,
		sessionID: sessionID,
		role:      role,
		options:   options,
		ctx:       ctx,
		cancel:    cancel,
		msgs:      make(chan *Message, options.MessageBuffer),
		errs:      make(chan error, 1),
	}
	ready := make(chan struct{})
	c.wg.Add(1)
	go c.run(ready)
	select {
	case <-ready:
		c.wg.Add(1)
		go c.heartbeatLoop()
		return c, nil
	case err := <-c.errs:
		cancel()
		return nil, err
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}
}

// Messages returns a stream of validated peer/server messages.
func (c *Client) Messages() <-chan *Message { return c.msgs }

// Errors returns terminal errors such as a failed reconnect window.
func (c *Client) Errors() <-chan error { return c.errs }

// Send writes one message. Trickle candidates must be sent in the caller's
// desired order; TCP and the writer mutex preserve that order.
func (c *Client) Send(m *Message) error {
	if m == nil {
		return ErrInvalidMessage
	}
	if m.SessionID == "" {
		m.SessionID = c.sessionID
	}
	if m.Role == "" {
		m.Role = c.role
	}
	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()
	if conn == nil {
		return ErrSignalTimeout
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(c.options.WriteTimeout))
	err := WriteMessage(conn, m)
	if err != nil {
		_ = conn.Close()
	}
	return err
}

func (c *Client) run(ready chan struct{}) {
	defer c.wg.Done()
	first := true
	for {
		if c.ctx.Err() != nil {
			return
		}
		conn, err := c.connectWithinWindow()
		if err != nil {
			c.report(err)
			return
		}
		c.connMu.Lock()
		c.conn = conn
		c.connMu.Unlock()
		if first {
			close(ready)
			first = false
		}
		c.readLoop(conn)
		c.connMu.Lock()
		if c.conn == conn {
			c.conn = nil
		}
		c.connMu.Unlock()
		_ = conn.Close()
		if c.ctx.Err() != nil || c.closed.Load() {
			return
		}
	}
}

func (c *Client) connectWithinWindow() (net.Conn, error) {
	deadline := time.Now().Add(c.options.ReconnectWindow)
	for {
		if c.ctx.Err() != nil {
			return nil, c.ctx.Err()
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("%w: could not connect to %s within %s", ErrSignalTimeout, c.addr, c.options.ReconnectWindow)
		}
		dialer := net.Dialer{Timeout: minDuration(remaining, 500*time.Millisecond), KeepAlive: 10 * time.Second}
		ctx, cancel := context.WithTimeout(c.ctx, minDuration(remaining, 500*time.Millisecond))
		conn, err := dialer.DialContext(ctx, "tcp", c.addr)
		cancel()
		if err == nil {
			if err := WriteMessage(conn, &Message{Type: TypeHello, SessionID: c.sessionID, Role: c.role}); err == nil {
				return conn, nil
			}
			_ = conn.Close()
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-c.ctx.Done():
			return nil, c.ctx.Err()
		}
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func (c *Client) readLoop(conn net.Conn) {
	reader := NewReader(conn)
	for {
		m, err := reader.Read()
		if err != nil {
			return
		}
		if err := Validate(m); err != nil {
			c.report(fmt.Errorf("signal: invalid server message: %w", err))
			continue
		}
		select {
		case c.msgs <- m:
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *Client) heartbeatLoop() {
	defer c.wg.Done()
	ticker := time.NewTicker(c.options.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			_ = c.Send(&Message{Type: TypeHeartbeat})
		}
	}
}

func (c *Client) report(err error) {
	if err == nil {
		return
	}
	select {
	case c.errs <- err:
	default:
	}
}

// Close stops reconnect attempts and closes the TCP stream.
func (c *Client) Close() error {
	c.closeOne.Do(func() {
		c.closed.Store(true)
		c.cancel()
		c.connMu.Lock()
		if c.conn != nil {
			_ = c.conn.Close()
			c.conn = nil
		}
		c.connMu.Unlock()
		c.wg.Wait()
		close(c.msgs)
	})
	return nil
}
