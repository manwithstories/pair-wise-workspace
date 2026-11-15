package relay

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	loopice "rtprelay/internal/ice"
	"rtprelay/internal/stats"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

type Faults struct {
	Drop      float64 `json:"drop"`
	Duplicate float64 `json:"duplicate"`
	Reorder   float64 `json:"reorder"`
}

func (f Faults) Enabled() bool {
	return f.Drop > 0 || f.Duplicate > 0 || f.Reorder > 0
}

type Config struct {
	Target      *net.UDPAddr
	Faults      Faults
	Seed        int64
	RewriteSSRC *uint32
	PacketLog   string
	IdleTimeout time.Duration
}

type Counts struct {
	Received    uint64 `json:"received"`
	Forwarded   uint64 `json:"forwarded"`
	Dropped     uint64 `json:"dropped"`
	Duplicated  uint64 `json:"duplicated"`
	Reordered   uint64 `json:"reordered"`
	WriteErrors uint64 `json:"write_errors"`
}

type Forwarder struct {
	cfg       Config
	conn      *net.UDPConn
	rng       *rand.Rand
	logFile   *os.File
	logWriter *bufio.Writer
	handleMu  sync.Mutex
	closed    bool

	received    atomic.Uint64
	forwarded   atomic.Uint64
	dropped     atomic.Uint64
	duplicated  atomic.Uint64
	reordered   atomic.Uint64
	writeErrors atomic.Uint64

	pending *pendingPacket
}

type pendingPacket struct {
	packet  *rtp.Packet
	arrival time.Time
}

type packetRecord struct {
	SSRC       uint32 `json:"ssrc"`
	Sequence   uint16 `json:"seq"`
	Timestamp  uint32 `json:"timestamp"`
	ReceivedNS int64  `json:"received_ns"`
	Action     string `json:"action"`
}

func New(cfg Config) (*Forwarder, error) {
	if cfg.Target == nil || cfg.Target.IP == nil || !loopice.IsLoopback(cfg.Target.IP.String()) {
		return nil, fmt.Errorf("relay target must be loopback")
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = time.Second
	}
	if err := validateFaults(cfg.Faults); err != nil {
		return nil, err
	}
	network := "udp4"
	if cfg.Target.IP.To4() == nil {
		network = "udp6"
	}
	conn, err := net.DialUDP(network, nil, cfg.Target)
	if err != nil {
		return nil, err
	}
	forwarder := &Forwarder{cfg: cfg, conn: conn, rng: rand.New(rand.NewSource(cfg.Seed))}
	if cfg.PacketLog != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.PacketLog), 0o755); err != nil && filepath.Dir(cfg.PacketLog) != "." {
			_ = conn.Close()
			return nil, err
		}
		file, err := os.Create(cfg.PacketLog)
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		forwarder.logFile = file
		forwarder.logWriter = bufio.NewWriterSize(file, 64*1024)
	}
	return forwarder, nil
}

func validateFaults(f Faults) error {
	for name, value := range map[string]float64{"drop": f.Drop, "duplicate": f.Duplicate, "reorder": f.Reorder} {
		if value < 0 || value > 1 {
			return fmt.Errorf("fault %s probability %.3f must be within [0,1]", name, value)
		}
	}
	return nil
}

func (f *Forwarder) Run(ctx context.Context, track *webrtc.TrackRemote, tracker *stats.Tracker, expected uint64) error {
	defer f.Flush()
	var processed uint64
	lastPacket := time.Now()
	for {
		if expected > 0 && processed >= expected {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return nil
		}
		_ = track.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		packet, _, err := track.ReadRTP()
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				if expected > 0 && processed < expected && time.Since(lastPacket) >= f.cfg.IdleTimeout {
					return nil
				}
				continue
			}
			return fmt.Errorf("read RTP: %w", err)
		}
		lastPacket = time.Now()
		arrival := time.Now()
		if tracker != nil {
			tracker.Update(packet.SequenceNumber, packet.Timestamp, arrival)
		}
		if err := f.Handle(packet, arrival); err != nil {
			return err
		}
		processed++
	}
}

func (f *Forwarder) Handle(packet *rtp.Packet, arrival time.Time) error {
	f.handleMu.Lock()
	defer f.handleMu.Unlock()
	if f.closed {
		return errors.New("forwarder is closed")
	}
	f.received.Add(1)
	action := "forward"
	if f.cfg.Faults.Drop > 0 && f.rng.Float64() < f.cfg.Faults.Drop {
		f.dropped.Add(1)
		return f.writeRecord(packet, arrival, "drop")
	}
	if f.pending != nil {
		if err := f.emit(packet); err != nil {
			return err
		}
		if err := f.emit(f.pending.packet); err != nil {
			return err
		}
		f.reordered.Add(1)
		f.pending = nil
		return f.writeRecord(packet, arrival, "reorder")
	}
	if f.cfg.Faults.Reorder > 0 && f.rng.Float64() < f.cfg.Faults.Reorder {
		f.pending = &pendingPacket{packet: packet, arrival: arrival}
		return f.writeRecord(packet, arrival, "reorder")
	}
	if err := f.emit(packet); err != nil {
		return err
	}
	if f.cfg.Faults.Duplicate > 0 && f.rng.Float64() < f.cfg.Faults.Duplicate {
		if err := f.emit(packet); err != nil {
			return err
		}
		f.duplicated.Add(1)
		action = "duplicate"
	}
	return f.writeRecord(packet, arrival, action)
}

func (f *Forwarder) Flush() {
	f.handleMu.Lock()
	defer f.handleMu.Unlock()
	f.flushLocked()
}

func (f *Forwarder) flushLocked() {
	if f.pending != nil {
		_ = f.emit(f.pending.packet)
		f.pending = nil
	}
	if f.logWriter != nil {
		_ = f.logWriter.Flush()
	}
}

func (f *Forwarder) emit(packet *rtp.Packet) error {
	out := *packet
	if f.cfg.RewriteSSRC != nil {
		out.SSRC = *f.cfg.RewriteSSRC
	}
	data, err := out.Marshal()
	if err != nil {
		f.writeErrors.Add(1)
		return err
	}
	if _, err := f.conn.Write(data); err != nil {
		f.writeErrors.Add(1)
		return err
	}
	f.forwarded.Add(1)
	return nil
}

func (f *Forwarder) writeRecord(packet *rtp.Packet, arrival time.Time, action string) error {
	if f.logWriter == nil {
		return nil
	}
	data, err := json.Marshal(packetRecord{
		SSRC: packet.SSRC, Sequence: packet.SequenceNumber, Timestamp: packet.Timestamp,
		ReceivedNS: arrival.UnixNano(), Action: action,
	})
	if err != nil {
		return err
	}
	if _, err := f.logWriter.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

func (f *Forwarder) Counts() Counts {
	return Counts{
		Received: f.received.Load(), Forwarded: f.forwarded.Load(),
		Dropped: f.dropped.Load(), Duplicated: f.duplicated.Load(),
		Reordered: f.reordered.Load(), WriteErrors: f.writeErrors.Load(),
	}
}

func (f *Forwarder) Close() error {
	f.handleMu.Lock()
	f.closed = true
	f.flushLocked()
	f.handleMu.Unlock()
	if f.logFile != nil {
		_ = f.logFile.Close()
	}
	if f.conn != nil {
		return f.conn.Close()
	}
	return nil
}
