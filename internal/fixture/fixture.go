// Package fixture generates deterministic MPEG-TS/RTP sample streams with
// optional transport faults. Every generator is seeded; identical seeds yield
// byte-identical streams.
package fixture

import (
	"fmt"
	"math/rand"

	"tsqc/internal/ts"
)

const (
	VideoPID uint16 = 0x0101
	AudioPID uint16 = 0x0102
	PMTPID   uint16 = 0x1000
)

// Options selects stream length and injected faults.
type Options struct {
	Seed        int64
	IDRs        int
	GOP         int // video frames per IDR
	AudioPerGOP int
	PayloadSize int
	Faults      []string
}

// Fault names accepted by ParseFaults.
const (
	FaultDuplicate = "duplicate"
	FaultReorder   = "reorder"
	FaultBurst     = "burst"
	FaultPCRWrap   = "pcrwrap"
	FaultCCJump    = "ccjump"
	FaultBadPSI    = "badpsi"
	FaultTruncate  = "truncate"
)

// AllFaults lists every injectable fault in deterministic order.
var AllFaults = []string{FaultDuplicate, FaultReorder, FaultBurst, FaultPCRWrap, FaultCCJump, FaultBadPSI, FaultTruncate}

// DefaultOptions returns the demo configuration used by acceptance runs. The
// RTP payload is a whole number of TS packets so a dropped packet never
// shifts transport alignment.
func DefaultOptions(seed int64) Options {
	return Options{Seed: seed, IDRs: 8, GOP: 4, AudioPerGOP: 3, PayloadSize: ts.PacketSize}
}

// Stream is one generated fixture: the transport stream plus the RTP view.
type Stream struct {
	TS      []byte
	Packets []RTPPacket
	Groups  []FECGroup
	Faults  []string
}

// RTPPacket is one RTP payload. Every payload is an integral number of TS
// packets, so loss cannot shift transport alignment.
type RTPPacket struct {
	Seq     uint16
	Payload []byte
	Marker  bool
	Key     bool   // payload covers part of an IDR access unit
	Raw     []byte // Pion-marshaled wire bytes
}

// FECGroup protects Count consecutive sequence numbers.
type FECGroup struct {
	Base    uint16
	Count   int
	Lengths []int
	Parity  []byte
}

// ParseFaults expands "all" and validates names.
func ParseFaults(names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	if len(names) == 1 && names[0] == "all" {
		return append([]string(nil), AllFaults...), nil
	}
	known := map[string]bool{}
	for _, f := range AllFaults {
		known[f] = true
	}
	var out []string
	for _, n := range names {
		if !known[n] {
			return nil, fmt.Errorf("unknown fault %q (valid: all, %v)", n, AllFaults)
		}
		out = append(out, n)
	}
	return out, nil
}

// Generate builds the base transport stream and applies the requested
// faults in a fixed order so the same seed and fault set always match.
type builder struct {
	opts   Options
	rng    *rand.Rand
	cc     map[uint16]uint8
	out    []byte
	pcr    int64
	pts    int64
	curKey bool
	pktKey []bool // per transport packet: part of an IDR access unit
}
