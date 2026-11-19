// Package verify validates remuxed output and simulates RTP packet loss
// to quantify recovery. Sequence numbers are 16-bit and wrap; edge cases
// cover wrap-around and consecutive loss runs.
package verify

import (
	"fmt"
	"math/rand"

	"github.com/pion/rtp"

	"aircheck/internal/fault"
	"aircheck/internal/scan"
)

// LossReport quantifies simulated RTP loss and recovery.
type LossReport struct {
	Sent              int        `json:"sent"`
	Lost              int        `json:"lost"`
	Recovered         int        `json:"recovered"`
	RecoveryRate      float64    `json:"recovery_rate_pct"`
	UnrecoveredRanges [][2]uint16 `json:"unrecovered_ranges,omitempty"`
}

// SimulateLoss maps packets onto RTP sequence numbers (starting at
// seqStart to exercise wrap-around), drops them at lossRate, and recovers
// lost packets in runs of up to maxRecoveryGap (retransmit window).
// Longer consecutive loss runs are reported as unrecovered ranges.
func SimulateLoss(count int, seqStart uint16, lossRate float64, seed int64, maxRecoveryGap int) LossReport {
	rep := LossReport{Sent: count}
	if count <= 0 {
		return rep
	}
	rng := rand.New(rand.NewSource(seed))
	lost := make([]bool, count)
	for i := 0; i < count; i++ {
		if rng.Float64() < lossRate {
			lost[i] = true
			rep.Lost++
		}
	}
	// Group consecutive losses into runs.
	for i := 0; i < count; {
		if !lost[i] {
			i++
			continue
		}
		j := i
		for j < count && lost[j] {
			j++
		}
		runLen := j - i
		if runLen <= maxRecoveryGap {
			rep.Recovered += runLen
		} else {
			rep.UnrecoveredRanges = append(rep.UnrecoveredRanges,
				[2]uint16{seqStart + uint16(i), seqStart + uint16(j - 1)})
		}
		i = j
	}
	if rep.Lost > 0 {
		rep.RecoveryRate = float64(rep.Recovered) * 100.0 / float64(rep.Lost)
	}
	return rep
}

// RoundTrip marshals and re-parses one RTP packet per sequence number to
// verify header integrity across the wrap boundary.
func RoundTrip(seqStart uint16, count int, ssrc uint32) error {
	for i := 0; i < count; i++ {
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				SequenceNumber: seqStart + uint16(i),
				Timestamp:      uint32(i) * 3000,
				SSRC:           ssrc,
			},
			Payload: []byte{byte(i), byte(i >> 8)},
		}
		raw, err := pkt.Marshal()
		if err != nil {
			return fmt.Errorf("marshal seq %d: %w", pkt.SequenceNumber, err)
		}
		var back rtp.Packet
		if err := back.Unmarshal(raw); err != nil {
			return fmt.Errorf("unmarshal seq %d: %w", pkt.SequenceNumber, err)
		}
		if back.SequenceNumber != pkt.SequenceNumber || back.SSRC != ssrc {
			return fmt.Errorf("round-trip mismatch at seq %d", pkt.SequenceNumber)
		}
	}
	return nil
}

// Summary is the comparable digest of a container used for output
// validation.
type Summary struct {
	Tracks       int   `json:"tracks"`
	VideoPackets int   `json:"video_packets"`
	AudioPackets int   `json:"audio_packets"`
	VideoKeys    int   `json:"video_keys"`
	DurationBase int64 `json:"duration_base"` // unified 90 kHz units
}

// Summarize builds the digest from parsed container info and packets.
func Summarize(ci *scan.ContainerInfo, byTrack map[uint32][]scan.Packet) Summary {
	var s Summary
	s.Tracks = len(ci.Tracks)
	for _, t := range ci.Tracks {
		pkts := byTrack[t.ID]
		switch t.MediaType {
		case "video":
			s.VideoPackets += len(pkts)
			for _, p := range pkts {
				if p.Keyframe {
					s.VideoKeys++
				}
			}
		case "audio":
			s.AudioPackets += len(pkts)
		}
		for _, p := range pkts {
			if p.DTS > s.DurationBase {
				s.DurationBase = p.DTS
			}
		}
	}
	return s
}

// CompareOutput re-parses the remuxed file and checks that tracks,
// timeline and keyframe counts match the source digest. Any mismatch
// returns a diagnostic fault (verification failure).
func CompareOutput(want Summary, outCI *scan.ContainerInfo, outByTrack map[uint32][]scan.Packet) fault.List {
	got := Summarize(outCI, outByTrack)
	var faults fault.List
	mismatch := func(field string, w, g int64) {
		faults = append(faults, fault.New(fault.KindVerifyMismatch, fault.StageVerify, 0, 0, -1, false,
			fmt.Sprintf("%s: want %d got %d", field, w, g)))
	}
	if int64(want.Tracks) != int64(got.Tracks) {
		mismatch("tracks", int64(want.Tracks), int64(got.Tracks))
	}
	if int64(want.VideoPackets) != int64(got.VideoPackets) {
		mismatch("video_packets", int64(want.VideoPackets), int64(got.VideoPackets))
	}
	if int64(want.AudioPackets) != int64(got.AudioPackets) {
		mismatch("audio_packets", int64(want.AudioPackets), int64(got.AudioPackets))
	}
	if int64(want.VideoKeys) != int64(got.VideoKeys) {
		mismatch("video_keys", int64(want.VideoKeys), int64(got.VideoKeys))
	}
	// Timeline: allow one sample of slack on the unified base.
	d := want.DurationBase - got.DurationBase
	if d < 0 {
		d = -d
	}
	if d > 3000 {
		mismatch("duration_base", want.DurationBase, got.DurationBase)
	}
	return faults
}
