package relay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
)

var (
	sinkProbePayload = []byte("relayctl-sink-probe-v1")
	sinkReadyPayload = []byte("relayctl-sink-ready-v1")
)

// ErrNonLoopbackTarget is returned for signalling/relay addresses that escape
// the local-only policy.
var ErrNonLoopbackTarget = errors.New("relay: non-loopback UDP target")

// Forwarder rewrites identity fields and forwards to one downstream UDP sink.
// Calls are serialized by the relay pipeline; it deliberately keeps no mutex.
type Forwarder struct {
	conn      udpWriter
	stats     *Stats
	ssrc      uint32
	startSeq  uint16
	successes uint64
	timeout   time.Duration
}

type udpWriter interface {
	Write([]byte) (int, error)
	SetWriteDeadline(time.Time) error
	RemoteAddr() net.Addr
	Close() error
}

// NewUDPForwarder resolves a loopback sink and prepares identity rewriting.
func NewUDPForwarder(target string, ssrc uint32, startSeq uint16, stats *Stats) (*Forwarder, error) {
	remote, err := ResolveLoopbackUDP(target)
	if err != nil {
		return nil, err
	}
	network := "udp4"
	if remote.IP.To4() == nil {
		network = "udp6"
	}
	conn, err := net.DialUDP(network, nil, remote)
	if err != nil {
		return nil, fmt.Errorf("dial UDP sink: %w", err)
	}
	if stats == nil {
		stats = NewStats()
	}
	return &Forwarder{conn: conn, stats: stats, ssrc: ssrc, startSeq: startSeq, timeout: 500 * time.Millisecond}, nil
}

func newForwarderWithWriter(writer udpWriter, ssrc uint32, startSeq uint16, stats *Stats) *Forwarder {
	if stats == nil {
		stats = NewStats()
	}
	return &Forwarder{conn: writer, stats: stats, ssrc: ssrc, startSeq: startSeq, timeout: 500 * time.Millisecond}
}

// ResolveLoopbackUDP validates that addr contains a literal loopback IP.
func ResolveLoopbackUDP(addr string) (*net.UDPAddr, error) {
	resolved, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("resolve UDP address: %w", err)
	}
	if resolved.IP == nil || !resolved.IP.IsLoopback() {
		return nil, fmt.Errorf("%w: %s", ErrNonLoopbackTarget, addr)
	}
	return resolved, nil
}

// Forward rewrites SSRC and sequence number while preserving the RTP timestamp
// (and therefore the inter-packet timestamp interval).
func (f *Forwarder) Forward(pkt *rtp.Packet) error {
	if pkt == nil {
		return errors.New("relay: nil RTP packet")
	}
	pkt.SSRC = f.ssrc
	pkt.SequenceNumber = f.startSeq + uint16(f.successes)
	data, err := pkt.Marshal()
	if err != nil {
		f.stats.IncForwardError()
		return fmt.Errorf("marshal forwarded RTP: %w", err)
	}
	var writeErr error
	for attempt := 0; attempt < 2; attempt++ {
		_ = f.conn.SetWriteDeadline(time.Now().Add(f.timeout))
		_, writeErr = f.conn.Write(data)
		if writeErr == nil {
			f.successes++
			f.stats.IncForwarded(len(data))
			return nil
		}
	}
	f.stats.IncForwardError()
	return fmt.Errorf("UDP forward after retry: %w", writeErr)
}

// Target returns the resolved sink address.
func (f *Forwarder) Target() string { return f.conn.RemoteAddr().String() }

// Close releases the downstream socket.
func (f *Forwarder) Close() error { return f.conn.Close() }

// Sink is a loopback UDP listener used by demo and the standalone sink command.
type Sink struct {
	conn    *net.UDPConn
	packets atomic.Uint64
	bytes   atomic.Uint64
}

// ListenSink binds a loopback UDP socket. Port zero is supported.
func ListenSink(addr string) (*Sink, error) {
	local, err := ResolveLoopbackUDP(addr)
	if err != nil {
		return nil, err
	}
	network := "udp4"
	if local.IP.To4() == nil {
		network = "udp6"
	}
	conn, err := net.ListenUDP(network, local)
	if err != nil {
		return nil, fmt.Errorf("listen UDP sink: %w", err)
	}
	return &Sink{conn: conn}, nil
}

// CheckSink performs the loopback readiness handshake used before external-sink
// demos. A UDP write alone is insufficient: operating systems may silently drop
// datagrams when no process owns the port.
func CheckSink(ctx context.Context, target string) error {
	remote, err := ResolveLoopbackUDP(target)
	if err != nil {
		return err
	}
	network := "udp4"
	if remote.IP.To4() == nil {
		network = "udp6"
	}
	conn, err := net.DialUDP(network, nil, remote)
	if err != nil {
		return fmt.Errorf("dial sink readiness probe: %w", err)
	}
	defer conn.Close()
	deadline := time.Now().Add(500 * time.Millisecond)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)
	if _, err := conn.Write(sinkProbePayload); err != nil {
		return fmt.Errorf("send sink readiness probe: %w", err)
	}
	buffer := make([]byte, len(sinkReadyPayload))
	n, err := conn.Read(buffer)
	if err != nil {
		return fmt.Errorf("sink readiness timeout: %w", err)
	}
	if string(buffer[:n]) != string(sinkReadyPayload) {
		return errors.New("sink readiness response mismatch")
	}
	return nil
}

// Addr returns the actual sink address when port zero was requested.
func (s *Sink) Addr() string { return s.conn.LocalAddr().String() }

// Run drains packets until the context deadline or socket close. It records
// packet and byte counts, but does not reinterpret RTP payloads.
func (s *Sink) Run(done <-chan struct{}) error {
	buffer := make([]byte, 2048)
	for {
		_ = s.conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, remote, err := s.conn.ReadFromUDP(buffer)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				select {
				case <-done:
					return nil
				default:
					continue
				}
			}
			select {
			case <-done:
				return nil
			default:
				return err
			}
		}
		if string(buffer[:n]) == string(sinkProbePayload) {
			_, _ = s.conn.WriteToUDP(sinkReadyPayload, remote)
			continue
		}
		s.packets.Add(1)
		s.bytes.Add(uint64(n))
	}
}

// Counts returns the sink's received packet and byte totals.
func (s *Sink) Counts() (packets, bytes uint64) { return s.packets.Load(), s.bytes.Load() }

// Close releases the sink socket.
func (s *Sink) Close() error { return s.conn.Close() }

// IsLoopbackHost is a small helper shared by CLI argument validation.
func IsLoopbackHost(host string) bool {
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
