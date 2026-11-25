// Package fixture generates all media samples in memory. No external media or
// service is used; the fixed seed makes runs reproducible.
package fixture

import (
	"bytes"

	"github.com/pairwise/mediapack/internal/demux"
	"github.com/pairwise/mediapack/internal/transport"
)

const (
	PMTPID        uint16 = 0x1000
	VideoPID      uint16 = 0x0101
	AudioPID      uint16 = 0x0102
	ProgramNumber uint16 = 1

	Clips         = 4
	FramesPerClip = 4    // one keyframe + three delta frames
	FrameTick     = 3600 // 40 ms at 90 kHz
	ClipTick      = FramesPerClip * FrameTick
	SampleRate    = 8000
	SilenceMS     = 80
)

// Sample is a generated transport sample set.
type Sample struct {
	Seed          int64
	Faults        map[string]bool
	CompleteTS    []byte // exact 188-byte packets
	TS            []byte // CompleteTS plus optional truncated tail
	RTPDatagrams  [][]byte
	PacketCount   int
	ExpectedClips int
}

// Has reports whether a named fault was requested.
func (s Sample) Has(name string) bool { return s.Faults[name] }

// Config controls fixture generation.
type Config struct {
	Seed   int64
	Faults []string
}

func faultSet(names []string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// TSBuilder maintains per-PID continuity counters.
type TSBuilder struct {
	buf bytes.Buffer
	cc  map[uint16]uint8
}

func NewTSBuilder() *TSBuilder {
	return &TSBuilder{cc: make(map[uint16]uint8)}
}

func (b *TSBuilder) Bytes() []byte { return b.buf.Bytes() }

// Append sends a packet with the next normal CC.
func (b *TSBuilder) Append(pid uint16, pusi bool, payload []byte, pcr *uint64) error {
	cc := b.cc[pid]
	return b.appendCC(cc, pid, pusi, payload, pcr, true)
}

// AppendExplicitCC sends a chosen CC and then resumes sequence after it. Used
// only by the deliberate CC-gap fixture.
func (b *TSBuilder) AppendExplicitCC(cc uint8, pid uint16, pusi bool, payload []byte, pcr *uint64) error {
	return b.appendCC(cc, pid, pusi, payload, pcr, false)
}

func (b *TSBuilder) appendCC(cc uint8, pid uint16, pusi bool, payload []byte, pcr *uint64, incrementFromCurrent bool) error {
	raw, err := transport.BuildPacket(transport.PacketOptions{
		PID: pid, PUSI: pusi, CC: cc, PCR: pcr, Payload: payload,
	})
	if err != nil {
		return err
	}
	if _, err := b.buf.Write(raw); err != nil {
		return err
	}
	if incrementFromCurrent {
		b.cc[pid] = (cc + 1) & 0x0f
	} else {
		b.cc[pid] = (cc + 1) & 0x0f
	}
	return nil
}

// AppendPES packetizes one PES into TS payloads. A non-nil PCR is placed on
// the first fragment; callers must keep such video PES below PCR capacity.
func (b *TSBuilder) AppendPES(pid uint16, streamID byte, ptsTicks uint64, media []byte, pcr *uint64) error {
	pes := demux.BuildPES(streamID, ptsTicks, media)
	first := true
	for start := 0; start < len(pes); {
		capacity := 184
		var pcrArg *uint64
		if first && pcr != nil {
			capacity = 176
			pcrArg = pcr
		}
		end := min(start+capacity, len(pes))
		if err := b.Append(pid, first, pes[start:end], pcrArg); err != nil {
			return err
		}
		start = end
		first = false
	}
	return nil
}
