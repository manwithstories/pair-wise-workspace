// Package fixture synthesizes deterministic, boundary-heavy relay inputs:
// reordered/duplicate RTP packets, single and burst loss, tfdt rollback,
// 33-bit timestamp wrap, VFR sample durations and truncated mdat. Everything
// is derived from a seed so repeat generation yields byte-identical output.
package fixture

import (
	"github.com/pion/rtp"
)

// Case names accepted by the fixture subcommand.
const (
	CaseDriftWrapVFR = "drift-wrap-vfr"
	CaseTruncateMdat = "truncate-mdat"
)

// TrackKind labels a synthetic track.
type TrackKind string

const (
	Video TrackKind = "video"
	Audio TrackKind = "audio"
)

// TrackSpec describes one generated track.
type TrackSpec struct {
	ID        uint32
	Kind      TrackKind
	Timescale uint32
	// Samples holds per-sample (ticksDuration, payloadSize).
	Samples []SampleSpec
	// BaseTfdt is the starting baseMediaDecodeTime in track ticks.
	BaseTfdt uint64
}

// SampleSpec is one VFR sample: duration in track ticks and payload bytes.
type SampleSpec struct {
	Duration uint32
	Size     uint32
}

// Packet is a generated RTP packet plus its intended sequence number, kept
// separately so the transport layer can reorder and deduplicate.
type Packet struct {
	Seq     uint16
	Packet  *rtp.Packet
	IsFEC   bool
}

// Manifest is the on-disk fixture descriptor written alongside the bytes.
type Manifest struct {
	Case          string       `json:"case"`
	Seed          int64        `json:"seed"`
	Tracks        []TrackSpec  `json:"tracks"`
	SequencePlan  SequencePlan `json:"sequence_plan"`
	// ExpectedDamageOffset is the first damaged byte for damage cases, and
	// the value the pipeline must locate exactly (delta must be 0).
	ExpectedDamageOffset int64 `json:"expected_damage_offset,omitempty"`
	// ExpectBlocked names the sync/DAG node expected to block.
	ExpectBlocked string `json:"expect_blocked,omitempty"`
	// ExpectUnrecoverable marks cases whose burst loss cannot be XOR-repaired.
	ExpectUnrecoverable bool `json:"expect_unrecoverable,omitempty"`
}

// SequencePlan records how the packet stream was permuted/lost.
type SequencePlan struct {
	Order        []uint16 `json:"order"`
	Duplicates   []uint16 `json:"duplicates,omitempty"`
	SingleLosses []uint16 `json:"single_losses,omitempty"`
	BurstLosses  []uint16 `json:"burst_losses,omitempty"`
	FECGroups    [][]uint16 `json:"fec_groups,omitempty"`
}

// rtpBuild encodes a media chunk as an RTP packet with a fixed SSRC and the
// given sequence number and RTP timestamp.
func rtpBuild(ssrc uint32, seq uint16, ts uint32, payload []byte) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SSRC:           ssrc,
			SequenceNumber: seq,
			Timestamp:      ts,
			Marker:         true,
		},
		Payload: append([]byte(nil), payload...),
	}
}
