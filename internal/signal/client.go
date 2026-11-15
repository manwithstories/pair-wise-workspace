package signal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

var ErrNotConnected = errors.New("signal client is not connected")

type ClientConfig struct {
	URL               string
	Stream            string
	Role              string
	HeartbeatInterval time.Duration
	HandshakeTimeout  time.Duration
	Reconnect         bool
}

type Client struct {
	cfg    ClientConfig
	source string

	mu           sync.Mutex
	conn         *websocket.Conn
	seq          uint64
	cache        *SequenceCache
	closedByUser bool

	writeMu   sync.Mutex
	connected atomic.Bool
	ready     chan struct{}
	readyOnce sync.Once
	closed    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
	errs      chan error

	onOffer   func(Offer)
	onAnswer  func(Answer)
	onTrickle func(Trickle)
}

func NewClient(cfg ClientConfig) *Client {
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = 10 * time.Second
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = 10 * time.Second
	}
	return &Client{
		cfg:    cfg,
		source: fmt.Sprintf("%s-%d-%d", cfg.Role, os.Getpid(), time.Now().UnixNano()),
		cache:  NewSequenceCache(),
		ready:  make(chan struct{}),
		closed: make(chan struct{}),
		errs:   make(chan error, 16),
	}
}

func (c *Client) OnOffer(handler func(Offer))   { c.onOffer = handler }
func (c *Client) OnAnswer(handler func(Answer)) { c.onAnswer = handler }
func (c *Client) OnTrickle(handler func(Trickle)) {
	c.onTrickle = handler
}

func (c *Client) Errors() <-chan error { return c.errs }
func (c *Client) Connected() bool      { return c.connected.Load() }

func (c *Client) Start() {
	c.wg.Add(1)
	go c.run()
}

func (c *Client) WaitReady(ctx context.Context) error {
	select {
	case <-c.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return errors.New("signal client closed before handshake")
	}
}

func (c *Client) Close() error {
	var closeErr error
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closedByUser = true
		conn := c.conn
		c.conn = nil
		c.mu.Unlock()
		close(c.closed)
		if conn != nil {
			_ = c.writeEnvelope(conn, Envelope{
				Version: ProtocolVersion, Type: TypeBye, Stream: c.cfg.Stream, Source: c.source,
			})
			closeErr = conn.Close()
		}
	})
	c.wg.Wait()
	return closeErr
}

func (c *Client) Send(messageType MessageType, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	conn, err := c.currentConn()
	if err != nil {
		return err
	}
	next := c.seq + 1
	env := Envelope{
		Version: ProtocolVersion, Seq: next, Type: messageType,
		Stream: c.cfg.Stream, Source: c.source, Payload: raw,
	}
	if err := c.writeEnvelope(conn, env); err != nil {
		return err
	}
	c.seq = next
	return nil
}

func (c *Client) dispatch(env Envelope) {
	switch env.Type {
	case TypeOffer:
		if c.onOffer != nil {
			var payload Offer
			if err := json.Unmarshal(env.Payload, &payload); err != nil {
				c.reportError(err)
				return
			}
			c.onOffer(payload)
		}
	case TypeAnswer:
		if c.onAnswer != nil {
			var payload Answer
			if err := json.Unmarshal(env.Payload, &payload); err != nil {
				c.reportError(err)
				return
			}
			c.onAnswer(payload)
		}
	case TypeTrickle:
		if c.onTrickle != nil {
			var payload Trickle
			if err := json.Unmarshal(env.Payload, &payload); err != nil {
				c.reportError(err)
				return
			}
			c.onTrickle(payload)
		}
	}
}

func (c *Client) writeEnvelope(conn *websocket.Conn, env Envelope) error {
	data, err := MarshalEnvelope(env)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return conn.WriteMessage(websocket.TextMessage, data)
}

func (c *Client) currentConn() (*websocket.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil, ErrNotConnected
	}
	return c.conn, nil
}

func (c *Client) sleep(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-c.closed:
		return false
	}
}

func (c *Client) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

func (c *Client) reportError(err error) {
	if err == nil || c.isClosed() {
		return
	}
	select {
	case c.errs <- err:
	default:
	}
}
