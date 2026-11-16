package signaling

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// serverConn wraps one accepted TCP connection with a serialized writer.
type serverConn struct {
	conn    net.Conn
	server  *Server
	id      string
	session string
	wmu     sync.Mutex
	closed  bool
}

func (c *serverConn) write(msg Message) error {
	raw, err := Encode(msg)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	// Bound the write so a stalled peer cannot wedge a handler that holds the
	// server lock.
	_ = c.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err = c.conn.Write(raw)
	return err
}

func (c *serverConn) close() {
	c.wmu.Lock()
	if c.closed {
		c.wmu.Unlock()
		return
	}
	c.closed = true
	c.wmu.Unlock()
	_ = c.conn.Close()
}

func newSessionID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Client is the dialing side of the signaling protocol. It assigns sequence
// numbers, so callers cannot accidentally reuse one.
type Client struct {
	conn   net.Conn
	reader *bufio.Reader
	seq    atomic.Uint64
	wmu    sync.Mutex
}

// Dial connects to a signaling server.
func Dial(addr string) (*Client, error) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	return &Client{conn: conn, reader: bufio.NewReaderSize(conn, 64*1024)}, nil
}

// Send assigns the next sequence number and writes the message.
func (c *Client) Send(msg Message) error {
	msg.Seq = c.seq.Add(1)
	return c.SendRaw(msg)
}

// SendRaw writes a message exactly as given. It exists so tests and fault
// injection can replay a sequence number to exercise duplicate handling.
func (c *Client) SendRaw(msg Message) error {
	raw, err := Encode(msg)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.conn.Write(raw)
	return err
}

// Recv reads the next message, honouring the optional deadline.
func (c *Client) Recv(deadline time.Time) (Message, error) {
	if !deadline.IsZero() {
		if err := c.conn.SetReadDeadline(deadline); err != nil {
			return Message{}, err
		}
	}
	line, err := readLine(c.reader, defaultMaxLineSize)
	if err != nil {
		return Message{}, err
	}
	return Decode(line)
}

// Close closes the underlying socket.
func (c *Client) Close() error {
	if err := c.conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

// IsLoopbackCandidate reports whether an SDP candidate attribute names a
// loopback address. It returns a human-readable reason when it does not, so
// the caller can record why the candidate was refused.
func IsLoopbackCandidate(cand string) (bool, string) {
	fields := strings.Fields(cand)
	for i, f := range fields {
		if strings.EqualFold(f, "candidate") || strings.HasPrefix(strings.ToLower(f), "candidate:") {
			// "candidate:<foundation> <component> <proto> <prio> <ip> <port> typ ..."
			if strings.Contains(f, ":") {
				fields = append(strings.SplitN(f, ":", 2)[1:], fields[i+1:]...)
			} else {
				fields = fields[i+1:]
			}
			break
		}
	}
	if len(fields) < 5 {
		return false, fmt.Sprintf("malformed candidate %q", cand)
	}
	addr := fields[4]
	ip := net.ParseIP(strings.Trim(addr, "[]"))
	if ip == nil {
		return false, fmt.Sprintf("candidate address %q is not an IP", addr)
	}
	if !ip.IsLoopback() {
		return false, fmt.Sprintf("candidate address %s is not loopback", ip)
	}
	return true, ""
}
