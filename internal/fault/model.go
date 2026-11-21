// Package fault defines the shared error model used by every subsystem:
// parser, clock, inspector, pipeline and repack writer. A Fault always
// carries the packet sequence number and, when known, the unified media
// time so that independently processed packet groups can be merged into
// one deterministic, ordered report.
package fault

import "fmt"

// Category classifies a fault. Categories are stable strings because they
// are printed in reports and compared in tests.
type Category string

const (
	// CatTruncation: input ended with a partial packet (<188 bytes).
	CatTruncation Category = "truncation"
	// CatSyncLoss: a packet did not start with the 0x47 sync byte.
	CatSyncLoss Category = "sync_loss"
	// CatBadPacketSize: an unsupported packet size was configured.
	CatBadPacketSize Category = "bad_packet_size"
	// CatCCGap: continuity counter jumped forward (missing packets).
	CatCCGap Category = "cc_gap"
	// CatTEI: transport error indicator was set on a packet.
	CatTEI Category = "tei"
	// CatPSIIncomplete: stream ended (or table was superseded) with a
	// partially assembled PSI section.
	CatPSIIncomplete Category = "psi_incomplete"
	// CatPSICRC: PSI section CRC-32/MPEG mismatch.
	CatPSICRC Category = "psi_crc"
	// CatPSIRollback: PSI version moved backwards; update rejected.
	CatPSIRollback Category = "psi_version_rollback"
	// CatTimeJump: unwrapped PCR/PTS moved backwards on the timeline.
	CatTimeJump Category = "time_nonmonotonic"
	// CatUnrecoverable: structural damage that voids the whole stream.
	CatUnrecoverable Category = "unrecoverable"
)

// UnknownTime marks faults that cannot be placed on the media timeline.
const UnknownTime int64 = -1

// Fault is one diagnosed anomaly.
type Fault struct {
	Category Category
	PID      uint16
	// PacketIndex is the 0-based sequence number of the packet at which
	// the fault was detected (for gaps: the first packet after the hole).
	PacketIndex uint64
	// MediaTime is the unified 90 kHz media time, or UnknownTime.
	MediaTime   int64
	Recoverable bool
	Detail      string
}

func (f Fault) String() string {
	return fmt.Sprintf("%s pid=0x%04x pkt=%d t=%d recoverable=%t %s",
		f.Category, f.PID, f.PacketIndex, f.MediaTime, f.Recoverable, f.Detail)
}

// List is a deterministic collection of faults ordered by packet index.
type List []Fault

func (l List) Len() int      { return len(l) }
func (l List) Swap(i, j int) { l[i], l[j] = l[j], l[i] }
func (l List) Less(i, j int) bool {
	if l[i].PacketIndex != l[j].PacketIndex {
		return l[i].PacketIndex < l[j].PacketIndex
	}
	return l[i].Category < l[j].Category
}

// Count returns how many faults of a category the list holds.
func (l List) Count(c Category) int {
	n := 0
	for _, f := range l {
		if f.Category == c {
			n++
		}
	}
	return n
}

// HasUnrecoverable reports whether any fault voids the stream.
func (l List) HasUnrecoverable() bool {
	for _, f := range l {
		if !f.Recoverable {
			return true
		}
	}
	return false
}
