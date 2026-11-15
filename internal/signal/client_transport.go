package signal

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/gorilla/websocket"
)

func (c *Client) run() {
	defer c.wg.Done()
	backoff := 100 * time.Millisecond
	for {
		select {
		case <-c.closed:
			return
		default:
		}
		conn, err := c.connect()
		if err != nil {
			if !c.cfg.Reconnect {
				c.reportError(err)
				return
			}
			if !c.sleep(backoff) {
				return
			}
			if backoff < time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 100 * time.Millisecond
		c.mu.Lock()
		if c.closedByUser {
			c.mu.Unlock()
			_ = conn.Close()
			return
		}
		c.conn = conn
		c.mu.Unlock()
		c.connected.Store(true)
		c.readyOnce.Do(func() { close(c.ready) })
		heartbeatDone := make(chan struct{})
		c.wg.Add(1)
		go c.heartbeat(conn, heartbeatDone)
		c.readLoop(conn)
		close(heartbeatDone)
		c.connected.Store(false)
		c.mu.Lock()
		if c.conn == conn {
			c.conn = nil
		}
		c.mu.Unlock()
		_ = conn.Close()
		if !c.cfg.Reconnect {
			return
		}
		if !c.sleep(backoff) {
			return
		}
	}
}

func (c *Client) connect() (*websocket.Conn, error) {
	url := fmt.Sprintf("%s?stream=%s&role=%s", c.cfg.URL, c.cfg.Stream, c.cfg.Role)
	dialer := websocket.Dialer{HandshakeTimeout: c.cfg.HandshakeTimeout}
	conn, _, err := dialer.Dial(url, nil)
	if err != nil {
		return nil, err
	}
	hello, _ := json.Marshal(Hello{Role: c.cfg.Role, Version: ProtocolVersion})
	err = c.writeEnvelope(conn, Envelope{
		Version: ProtocolVersion, Type: TypeHello, Stream: c.cfg.Stream,
		Source: c.source, Payload: hello,
	})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(c.cfg.HandshakeTimeout))
	_, data, err := conn.ReadMessage()
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	env, err := UnmarshalEnvelope(data)
	if err != nil || env.Type != TypeWelcome {
		_ = conn.Close()
		return nil, fmt.Errorf("invalid signal welcome: %w", err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	return conn, nil
}

func (c *Client) readLoop(conn *websocket.Conn) {
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if !c.isClosed() {
				c.reportError(err)
			}
			return
		}
		env, err := UnmarshalEnvelope(data)
		if err != nil {
			c.reportError(err)
			continue
		}
		if env.Type == TypeWelcome || env.Type == TypeHeartbeat {
			continue
		}
		ready, accepted := c.cache.Accept(env)
		if !accepted {
			continue
		}
		for _, item := range ready {
			c.dispatch(item)
		}
	}
}

func (c *Client) heartbeat(conn *websocket.Conn, done <-chan struct{}) {
	defer c.wg.Done()
	ticker := time.NewTicker(c.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-c.closed:
			return
		case <-ticker.C:
			if err := c.writeEnvelope(conn, Envelope{
				Version: ProtocolVersion, Type: TypeHeartbeat, Stream: c.cfg.Stream, Source: c.source,
			}); err != nil {
				_ = conn.Close()
				return
			}
		}
	}
}
