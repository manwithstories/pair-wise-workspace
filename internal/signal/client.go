package signal

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// ClientStats captures local protocol validation counters.
type ClientStats struct {
	Sent             uint64 `json:"sent"`
	Received         uint64 `json:"received"`
	RejectedSequence uint64 `json:"rejected_sequence"`
}

// Client is a reconnecting line-delimited JSON signaling client.
type Client struct {
	address string
	role    string
	room    string

	mu          sync.Mutex
	conn        net.Conn
	writer      *bufio.Writer
	endpoint    string
	seq         uint64
	inSeq       uint64
	closed      bool
	received    uint64
	rejectedSeq uint64
	incoming    chan Message
	errs        chan error
}

// Dial connects and joins a room. Subscribers may receive an offer immediately
// after joined; Recv returns that offer next.
func Dial(ctx context.Context, address, role, room string) (*Client, Message, error) {
	client := &Client{
		address:  address,
		role:     role,
		room:     room,
		incoming: make(chan Message, 32),
		errs:     make(chan error, 1),
	}
	joined, err := client.join(ctx, "")
	if err != nil {
		return nil, Message{}, err
	}
	return client, joined, nil
}

func (c *Client) join(ctx context.Context, endpoint string) (Message, error) {
	dialer := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", c.address)
	if err != nil {
		return Message{}, err
	}
	c.mu.Lock()
	old := c.conn
	c.conn = conn
	c.writer = bufio.NewWriter(conn)
	c.endpoint = endpoint
	c.inSeq = 0
	c.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	request := Message{Type: TypeJoin, Room: c.room, Role: c.role, Endpoint: endpoint}
	if err := c.Send(request); err != nil {
		_ = conn.Close()
		return Message{}, err
	}
	deadline := time.Now().Add(defaultHandshake)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetReadDeadline(deadline)
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		_ = conn.Close()
		return Message{}, fmt.Errorf("read joined: %w", err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	var response Message
	if err := json.Unmarshal(line, &response); err != nil {
		_ = conn.Close()
		return Message{}, err
	}
	if response.Seq <= c.inSeq {
		_ = conn.Close()
		return Message{}, fmt.Errorf("server sequence regression")
	}
	c.inSeq = response.Seq
	if response.Type == TypeError {
		_ = conn.Close()
		return Message{}, &RemoteError{Code: response.Code, Message: response.Error}
	}
	if response.Type != TypeJoined {
		_ = conn.Close()
		return Message{}, fmt.Errorf("expected joined, got %s", response.Type)
	}
	c.mu.Lock()
	c.endpoint = response.Endpoint
	c.mu.Unlock()
	go c.readLoop(conn, reader)
	return response, nil
}

func (c *Client) readLoop(conn net.Conn, reader *bufio.Reader) {
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			c.mu.Lock()
			isCurrent := c.conn == conn
			closed := c.closed
			c.mu.Unlock()
			if isCurrent && !closed {
				c.reportError(err)
			}
			return
		}
		var message Message
		if err := json.Unmarshal(line, &message); err != nil {
			c.rejectSequence()
			continue
		}
		c.mu.Lock()
		if message.Seq <= c.inSeq {
			c.rejectedSeq++
			c.mu.Unlock()
			continue
		}
		c.inSeq = message.Seq
		c.received++
		c.mu.Unlock()
		select {
		case c.incoming <- message:
		case <-time.After(defaultHandshake):
			c.reportError(fmt.Errorf("signaling receive queue stalled"))
			return
		}
	}
}

// Send writes one message with the next sequence number.
func (c *Client) Send(message Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil || c.closed {
		return net.ErrClosed
	}
	c.seq++
	message.Seq = c.seq
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := c.writer.Write(data); err != nil {
		return err
	}
	return c.writer.Flush()
}

// Recv waits for the next server message.
func (c *Client) Recv(ctx context.Context) (Message, error) {
	select {
	case <-ctx.Done():
		return Message{}, ctx.Err()
	case err := <-c.errs:
		return Message{}, err
	case message := <-c.incoming:
		if message.Type == TypeError {
			return message, &RemoteError{Code: message.Code, Message: message.Error}
		}
		return message, nil
	}
}

// Reconnect re-establishes TCP and resumes the allocated endpoint ID.
func (c *Client) Reconnect(ctx context.Context) (Message, error) {
	c.mu.Lock()
	endpoint := c.endpoint
	old := c.conn
	c.conn = nil
	c.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	if endpoint == "" {
		return Message{}, fmt.Errorf("cannot reconnect before join")
	}
	return c.join(ctx, endpoint)
}

// Endpoint returns the server-assigned endpoint identifier.
func (c *Client) Endpoint() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.endpoint
}

// Stats returns client counters.
func (c *Client) Stats() ClientStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return ClientStats{Sent: c.seq, Received: c.received, RejectedSequence: c.rejectedSeq}
}

// Close sends a best-effort bye and closes the socket.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	conn := c.conn
	c.mu.Unlock()
	if conn != nil {
		_ = c.Send(Message{Type: TypeBye})
	}
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	if conn == nil {
		return nil
	}
	return conn.Close()
}

func (c *Client) rejectSequence() {
	c.mu.Lock()
	c.rejectedSeq++
	c.mu.Unlock()
}

func (c *Client) reportError(err error) {
	if errors.Is(err, net.ErrClosed) {
		return
	}
	select {
	case c.errs <- err:
	default:
	}
}
