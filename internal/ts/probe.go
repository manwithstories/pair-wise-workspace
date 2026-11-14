package ts

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"time"
)

const (
	PacketSize       = 188
	PCRClock         = int64(27_000_000)
	defaultFrameRate = 30
)

// PCR is a decoded 27 MHz program clock reference.
type PCR struct {
	Base      uint64
	Extension uint16
}

// Ticks returns the 27 MHz PCR value.
func (p PCR) Ticks() int64 {
	return int64(p.Base*300 + uint64(p.Extension))
}

// Probe is the single-pass result for one transport stream segment.
type Probe struct {
	Path              string
	Size              int64
	Packets           int64
	TailBytes         int
	Truncated         bool
	SyncErrors        int64
	FirstPCR          int64
	LastPCR           int64
	HasPCR            bool
	FrameInterval     time.Duration
	Keyframes         []int64
	RandomAccessCount int64
}

// Duration is the measured playable duration, including one frame interval.
func (p *Probe) Duration() time.Duration {
	if !p.HasPCR || p.LastPCR <= p.FirstPCR {
		return 0
	}
	return TicksToDuration(p.LastPCR - p.FirstPCR + p.frameIntervalTicks())
}

// PCRSpan is the raw 27 MHz distance between the first and last PCR.
func (p *Probe) PCRSpan() int64 {
	if !p.HasPCR || p.LastPCR < p.FirstPCR {
		return 0
	}
	return p.LastPCR - p.FirstPCR
}

func (p *Probe) frameIntervalTicks() int64 {
	if p.FrameInterval > 0 {
		return DurationToTicks(p.FrameInterval)
	}
	return PCRClock / defaultFrameRate
}

// TicksToDuration converts 27 MHz ticks without overflowing a Duration.
func TicksToDuration(ticks int64) time.Duration {
	if ticks <= 0 {
		return 0
	}
	return time.Duration(float64(ticks) * float64(time.Second) / float64(PCRClock))
}

// DurationToTicks converts a duration to 27 MHz ticks.
func DurationToTicks(d time.Duration) int64 {
	return int64(float64(d) * float64(PCRClock) / float64(time.Second))
}

// ProbeFile scans a segment without loading the whole file into memory.
func ProbeFile(path string) (*Probe, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	p, err := ProbeReader(f)
	if err != nil {
		return nil, err
	}
	p.Path = path
	p.Size = info.Size()
	p.TailBytes = int(info.Size() % PacketSize)
	if p.TailBytes != 0 {
		p.Truncated = true
	}
	return p, nil
}

// ProbeReader scans 188-byte packets from r.
func ProbeReader(r io.Reader) (*Probe, error) {
	p := &Probe{}
	br := bufio.NewReaderSize(r, 64*1024)
	packet := make([]byte, PacketSize)
	var prevPCR int64
	var minPCRDelta int64
	var lastKeyframe int64
	for {
		n, err := io.ReadFull(br, packet)
		if err == io.EOF {
			break
		}
		if err == io.ErrUnexpectedEOF {
			p.Truncated = true
			p.TailBytes = n
			break
		}
		if err != nil {
			return nil, err
		}
		p.Packets++
		if packet[0] != 0x47 {
			p.SyncErrors++
			continue
		}
		hasPCR, pcr, randomAccess, err := parsePacket(packet)
		if err != nil {
			p.SyncErrors++
			continue
		}
		if bytes.Contains(packet, []byte{0x00, 0x00, 0x01, 0x65}) {
			randomAccess = true
		}
		if randomAccess {
			p.RandomAccessCount++
		}
		if !hasPCR {
			continue
		}
		ticks := pcr.Ticks()
		if !p.HasPCR {
			p.FirstPCR = ticks
			p.HasPCR = true
		}
		if prevPCR > 0 {
			if delta := ticks - prevPCR; delta > 0 && (minPCRDelta == 0 || delta < minPCRDelta) {
				minPCRDelta = delta
			}
		}
		prevPCR = ticks
		p.LastPCR = ticks
		if randomAccess && (len(p.Keyframes) == 0 || lastKeyframe != ticks) {
			p.Keyframes = append(p.Keyframes, ticks)
			lastKeyframe = ticks
		}
	}
	if minPCRDelta > 0 && minPCRDelta < PCRClock {
		p.FrameInterval = TicksToDuration(minPCRDelta)
	}
	return p, nil
}

func parsePacket(packet []byte) (bool, PCR, bool, error) {
	if len(packet) != PacketSize || packet[0] != 0x47 {
		return false, PCR{}, false, fmt.Errorf("invalid TS packet")
	}
	control := (packet[3] >> 4) & 0x03
	if control == 0 {
		return false, PCR{}, false, fmt.Errorf("invalid adaptation control")
	}
	hasAdaptation := control == 2 || control == 3
	if !hasAdaptation {
		return false, PCR{}, false, nil
	}
	length := int(packet[4])
	if length < 1 || 5+length > PacketSize {
		return false, PCR{}, false, fmt.Errorf("invalid adaptation length")
	}
	flags := packet[5]
	randomAccess := flags&0x40 != 0
	if flags&0x10 == 0 || length < 7 {
		return false, PCR{}, randomAccess, nil
	}
	b := packet[6:12]
	base := uint64(b[0])<<25 | uint64(b[1])<<17 | uint64(b[2])<<9 | uint64(b[3])<<1 | uint64(b[4])>>7
	ext := uint16(b[4]&0x01)<<8 | uint16(b[5])
	return true, PCR{Base: base, Extension: ext}, randomAccess, nil
}
