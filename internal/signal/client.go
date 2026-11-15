package signal

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// ErrHandshakeTimeout is returned when an offer/answer exchange does not
// complete within the configured timeout and retry budget.
var ErrHandshakeTimeout = errors.New("signal: handshake timeout")

// Client is a signaling endpoint with a handshake state machine.
type Client struct {
	conn    net.Conn
	id      string
	session string

	wmu sync.Mutex
	seq uint64

	inbox chan Message
	cand  chan string

	closeOnce sync.Once
	closed    chan struct{}
}

// Dial connects to the signaling server at addr and registers id in session.
func Dial(addr, session, id, role string) (*Client, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("signal: dial %s: %w", addr, err)
	}
	c := &Client{
		conn:    conn,
		id:      id,
		session: session,
		inbox:   make(chan Message, 128),
		cand:    make(chan string, 256),
		closed:  make(chan struct{}),
	}
	if err := c.send(Message{Kind: KindHello, Role: role}); err != nil {
		conn.Close()
		return nil, err
	}
	go c.readLoop()
	return c, nil
}

// ID returns the client identifier.
func (c *Client) ID() string { return c.id }

// Close terminates the connection.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.conn.Close()
	})
}

func (c *Client) send(m Message) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.seq++
	m.Seq = c.seq
	m.Session = c.session
	m.From = c.id
	return Encode(c.conn, m)
}

func (c *Client) readLoop() {
	r := bufio.NewReader(c.conn)
	for {
		m, err := Decode(r)
		if err != nil {
			return
		}
		if m.Kind == KindCandidate {
			select {
			case c.cand <- m.Candidate:
			default:
			}
			continue
		}
		select {
		case c.inbox <- m:
		case <-c.closed:
			return
		}
	}
}

// Candidates returns trickled ICE candidate strings from peers.
func (c *Client) Candidates() <-chan string { return c.cand }

// SendCandidate trickles one ICE candidate to a peer.
func (c *Client) SendCandidate(to, cand string) error {
	return c.send(Message{Kind: KindCandidate, To: to, Candidate: cand})
}

// SendSeq sends a message with an explicit sequence number. It exists to
// reproduce stale or duplicated traffic when testing rejection logic.
func (c *Client) SendSeq(seq uint64, m Message) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	m.Seq = seq
	m.Session = c.session
	m.From = c.id
	return Encode(c.conn, m)
}

// Recv waits for the next non-candidate message.
func (c *Client) Recv(ctx context.Context) (Message, error) {
	select {
	case m := <-c.inbox:
		return m, nil
	case <-ctx.Done():
		return Message{}, ctx.Err()
	case <-c.closed:
		return Message{}, net.ErrClosed
	}
}

// Offer runs the offerer side of the handshake: it sends the offer and waits
// for the matching answer, retrying with a fresh sequence number on timeout.
func (c *Client) Offer(ctx context.Context, to, sdp string, retries int, timeout time.Duration) (string, error) {
	for attempt := 0; ; attempt++ {
		if err := c.send(Message{Kind: KindOffer, To: to, SDP: sdp}); err != nil {
			return "", err
		}
		waitCtx, cancel := context.WithTimeout(ctx, timeout)
		m, err := c.waitKind(waitCtx, KindAnswer, to)
		cancel()
		if err == nil {
			return m.SDP, nil
		}
		if errors.Is(err, context.DeadlineExceeded) && attempt < retries {
			continue
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return "", ErrHandshakeTimeout
		}
		return "", err
	}
}

// AwaitOffer runs the answerer side: wait for an offer addressed to us.
func (c *Client) AwaitOffer(ctx context.Context) (from, sdp string, err error) {
	m, err := c.waitKind(ctx, KindOffer, "")
	if err != nil {
		return "", "", err
	}
	return m.From, m.SDP, nil
}

// Answer sends the answer back to the offerer.
func (c *Client) Answer(to, sdp string) error {
	return c.send(Message{Kind: KindAnswer, To: to, SDP: sdp})
}

// Bye notifies peers that this endpoint is leaving.
func (c *Client) Bye() error { return c.send(Message{Kind: KindBye}) }

func (c *Client) waitKind(ctx context.Context, k Kind, from string) (Message, error) {
	for {
		select {
		case m := <-c.inbox:
			if m.Kind != k {
				continue
			}
			if from != "" && m.From != from {
				continue
			}
			return m, nil
		case <-ctx.Done():
			return Message{}, ctx.Err()
		case <-c.closed:
			return Message{}, net.ErrClosed
		}
	}
}
