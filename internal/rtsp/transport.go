package rtsp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/pion/rtcp"
)

// ErrUDPSilence means no media arrived within the configured UDP threshold.
var ErrUDPSilence = errors.New("udp media silence timeout")

// Transport exposes only RTSP control bytes, RTP media bytes, and errors.
// It never decides session state.
type Transport interface {
	SendControl(context.Context, []byte) error
	ReceiveControl(context.Context) ([]byte, error)
	ReceiveMedia(context.Context) ([]byte, error)
	Close() error
}

// ValidateLoopbackAddr rejects anything except an IPv4 loopback address.
func ValidateLoopbackAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.Equal(net.IPv4(127, 0, 0, 1)) {
		return fmt.Errorf("only 127.0.0.1 is allowed, got %q", addr)
	}
	return nil
}

// TCPTransport multiplexes RTSP control and interleaved RTP/RTCP on one socket.
type TCPTransport struct {
	conn      net.Conn
	parser    *InterleavedParser
	control   chan []byte
	media     chan []byte
	done      chan struct{}
	mediaDone chan struct{}
	doneOnce  sync.Once
	mediaOnce sync.Once
	closeOnce sync.Once
	writeMu   sync.Mutex
	mu        sync.Mutex
	readErr   error
}

// DialTCP connects to a loopback RTSP endpoint and starts its stream parser.
func DialTCP(ctx context.Context, addr string) (*TCPTransport, error) {
	if err := ValidateLoopbackAddr(addr); err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp4", addr)
	if err != nil {
		return nil, err
	}
	t := &TCPTransport{
		conn:      conn,
		parser:    NewInterleavedParser(),
		control:   make(chan []byte, 32),
		media:     make(chan []byte, 256),
		done:      make(chan struct{}),
		mediaDone: make(chan struct{}),
	}
	go t.readLoop()
	return t, nil
}

func (t *TCPTransport) readLoop() {
	buf := make([]byte, 32<<10)
	for {
		n, err := t.conn.Read(buf)
		if n > 0 {
			chunks, parseErr := t.parser.Feed(buf[:n])
			if parseErr != nil {
				t.fail(parseErr)
				return
			}
			for _, chunk := range chunks {
				if !t.dispatch(chunk) {
					return
				}
			}
		}
		if err != nil {
			t.fail(err)
			return
		}
	}
}

func (t *TCPTransport) dispatch(chunk Chunk) bool {
	switch chunk.Kind {
	case ChunkControl:
		select {
		case t.control <- chunk.Data:
			return true
		case <-t.done:
			return false
		}
	case ChunkRTP:
		select {
		case t.media <- chunk.Data:
			return true
		case <-t.done:
			return false
		}
	case ChunkRTCP:
		packets, err := rtcp.Unmarshal(chunk.Data)
		if err != nil {
			t.fail(fmt.Errorf("parse RTCP: %w", err))
			return false
		}
		for _, packet := range packets {
			if _, ok := packet.(*rtcp.Goodbye); ok {
				t.mediaOnce.Do(func() { close(t.mediaDone) })
			}
		}
		return true
	default:
		t.fail(fmt.Errorf("unknown chunk kind %d", chunk.Kind))
		return false
	}
}

// SendControl writes one complete RTSP request.
func (t *TCPTransport) SendControl(ctx context.Context, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if err := t.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	defer t.conn.SetWriteDeadline(time.Time{})
	_, err := t.conn.Write(data)
	return err
}

// ReceiveControl returns one complete RTSP response.
func (t *TCPTransport) ReceiveControl(ctx context.Context) ([]byte, error) {
	select {
	case data := <-t.control:
		return data, nil
	default:
	}
	select {
	case data := <-t.control:
		return data, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.done:
		select {
		case data := <-t.control:
			return data, nil
		default:
			return nil, t.terminalError()
		}
	}
}

// ReceiveMedia returns one RTP datagram or io.EOF after RTCP BYE.
func (t *TCPTransport) ReceiveMedia(ctx context.Context) ([]byte, error) {
	select {
	case data := <-t.media:
		return data, nil
	default:
	}
	select {
	case data := <-t.media:
		return data, nil
	case <-t.mediaDone:
		select {
		case data := <-t.media:
			return data, nil
		default:
			return nil, io.EOF
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.done:
		select {
		case data := <-t.media:
			return data, nil
		default:
			return nil, t.terminalError()
		}
	}
}

func (t *TCPTransport) fail(err error) {
	t.mu.Lock()
	if t.readErr == nil {
		t.readErr = err
	}
	t.mu.Unlock()
	t.doneOnce.Do(func() { close(t.done) })
}

func (t *TCPTransport) terminalError() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.readErr != nil {
		return t.readErr
	}
	return io.EOF
}

// Close releases the TCP socket and unblocks pending receives.
func (t *TCPTransport) Close() error {
	var err error
	t.closeOnce.Do(func() {
		t.doneOnce.Do(func() { close(t.done) })
		err = t.conn.Close()
	})
	return err
}
