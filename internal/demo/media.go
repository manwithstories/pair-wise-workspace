package demo

import (
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"

	"github.com/pion/rtp"

	"archivectl/internal/layout"
	"archivectl/internal/repair"
	"archivectl/internal/scan"
)

// writeTrackSegments generates n one-second fragments per track.
//
// clean:   v1 tfdt, arbitrary non-zero but continuous start.
// damaged: video uses v0 tfdt starting just below 2^32 to force a wrap,
//
//	and declared discontinuities reset both producer clocks.
func writeTrackSegments(root, group, track string, n int, damaged bool, disc map[int]bool) error {
	dur := layout.DurationTick(track)
	count := layout.SamplesPerSegment(track)
	segDur := uint64(dur) * uint64(count)

	clock := int64(cleanStart(track))
	if damaged && track == "video" {
		clock = 0xFFFF0000 // ~one 90 kHz second below the 32-bit boundary
	}

	for i := 0; i < n; i++ {
		if damaged && disc[i] && i > 0 {
			clock = resetBase(track) // producer restart at declared boundary
		}
		v1 := true
		if damaged && track == "video" {
			v1 = false
		}
		base := uint64(clock)
		spec := scan.FragmentSpec{TrackID: trackID(track), Sequence: uint32(i + 1), BaseDecodeTime: base, TFDTv1: v1}
		for s := 0; s < count; s++ {
			spec.Samples = append(spec.Samples, scan.SampleSpec{Duration: dur, Size: payloadSize(track)})
		}
		b := scan.BuildFragment(spec)
		p := layout.SourcePath(root, group, track, i)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return err
		}
		clock += int64(segDur)
	}
	return nil
}

func cleanStart(track string) int64 {
	if track == "audio" {
		return 1_920_000
	}
	return 3_600_000
}
func resetBase(track string) int64 {
	if track == "audio" {
		return 555
	}
	return 777
}
func trackID(track string) uint32 {
	if track == "audio" {
		return 2
	}
	return 1
}
func payloadSize(track string) uint32 {
	if track == "audio" {
		return 12
	}
	return 24
}

// rtpOutcome records the exact injected loss budget for the oracle.
type rtpOutcome struct {
	Groups             int
	TotalLost          int
	Recovered          int
	UnrecoveredGaps    int
	UnrecoveredPackets int
}

// writeRTP builds the length-prefixed captured RTP stream and the
// side-band XOR FEC records. The damaged profile injects duplicates,
// reordering, 28 recoverable single losses and one double loss.
func writeRTP(root, group string, damaged bool, seed int64) (rtpOutcome, error) {
	rng := rand.New(rand.NewSource(seed + 7))
	payloadLen := 32
	startSeq := uint16(65516)

	groupCount := 4
	if damaged {
		groupCount = 30
	}
	type media struct {
		seq     uint16
		payload []byte
	}
	var all []media
	var groups []repair.FECGroup
	for g := 0; g < groupCount; g++ {
		seqs := make([]uint16, 4)
		payloads := make([][]byte, 4)
		for k := 0; k < 4; k++ {
			seq := startSeq + uint16(4*g+k)
			p := make([]byte, payloadLen)
			rng.Read(p)
			// Make payloads deterministic-ish but distinct per seq.
			for j := range p {
				p[j] ^= byte(seq) ^ byte(j*13+g)
			}
			seqs[k] = seq
			payloads[k] = p
			all = append(all, media{seq, p})
		}
		groups = append(groups, repair.FECGroup{Index: g, Sequences: seqs, Parity: repair.XORPayload(payloads)})
	}

	// Decide losses.
	lost := map[uint16]bool{}
	var oc rtpOutcome
	oc.Groups = groupCount
	if damaged {
		for g := 0; g < 28; g++ {
			seq := groups[g].Sequences[g%4] // exactly one missing per group
			lost[seq] = true
		}
		// One double loss in group 28, adjacent members -> one gap of 2.
		lost[groups[28].Sequences[1]] = true
		lost[groups[28].Sequences[2]] = true
		oc.TotalLost = 30
		oc.Recovered = 28
		oc.UnrecoveredGaps = 1
		oc.UnrecoveredPackets = 2
	}

	payloadBySeq := map[uint16][]byte{}
	for _, m := range all {
		payloadBySeq[m.seq] = m.payload
	}

	var captured []rtp.Packet
	for _, m := range all {
		if lost[m.seq] {
			continue
		}
		captured = append(captured, rtp.Packet{
			Header:  rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: m.seq, Timestamp: uint32(m.seq) * 3000, SSRC: 0x41554449},
			Payload: m.payload,
		})
	}
	if damaged && len(captured) > 6 {
		// Duplicate one packet.
		captured = append(captured, captured[3])
		// Reorder two adjacent packets (swap positions 5 and 6).
		captured[5], captured[6] = captured[6], captured[5]
	}

	bin, err := repair.MarshalCapture(captured)
	if err != nil {
		return oc, err
	}
	capPath := layout.CapturePath(root, group)
	if err := os.MkdirAll(filepath.Dir(capPath), 0o755); err != nil {
		return oc, err
	}
	if err := os.WriteFile(capPath, bin, 0o644); err != nil {
		return oc, err
	}
	fecBytes, err := json.MarshalIndent(groups, "", "  ")
	if err != nil {
		return oc, err
	}
	if err := os.WriteFile(layout.FECPath(root, group), fecBytes, 0o644); err != nil {
		return oc, err
	}
	return oc, nil
}
