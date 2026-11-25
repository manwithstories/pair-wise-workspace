package repair_test

import (
	"bytes"
	"testing"

	"github.com/pion/rtp"

	"archivectl/internal/repair"
	"archivectl/internal/scan"
)

func pkt(seq uint16, payload []byte) rtp.Packet {
	return rtp.Packet{Header: rtp.Header{SequenceNumber: seq}, Payload: payload}
}

func xorGroup(seqs []uint16, payloads map[uint16][]byte) repair.FECGroup {
	var ps [][]byte
	for _, s := range seqs {
		ps = append(ps, payloads[s])
	}
	return repair.FECGroup{Sequences: seqs, Parity: repair.XORPayload(ps)}
}

func TestRTPReorderDuplicateFECAndDoubleLoss(t *testing.T) {
	payloads := map[uint16][]byte{}
	var all []rtp.Packet
	for s := uint16(100); s <= 107; s++ {
		p := bytes.Repeat([]byte{byte(s)}, 8)
		payloads[s] = p
		all = append(all, pkt(s, p))
	}
	gA := xorGroup([]uint16{100, 101, 102, 103}, payloads)
	gB := xorGroup([]uint16{104, 105, 106, 107}, payloads)

	// Capture order: swap 103/104 (reorder), duplicate 100, and omit
	// 102 (single -> recoverable) plus 105,106 (double -> gap).
	var captured []rtp.Packet
	keep := func(s uint16) {
		if s == 102 || s == 105 || s == 106 {
			return
		}
		captured = append(captured, pkt(s, payloads[s]))
	}
	keep(100)
	captured = append(captured, pkt(100, payloads[100])) // duplicate
	keep(101)
	captured = append(captured, pkt(104, payloads[104])) // swapped forward
	keep(103)
	for s := uint16(104); s <= 107; s++ {
		if s == 104 {
			continue
		}
		keep(s)
	}

	rep := repair.RepairRTP(captured, []repair.FECGroup{gA, gB})
	if rep.Duplicates != 1 {
		t.Fatalf("duplicates=%d want 1", rep.Duplicates)
	}
	if rep.Reordered < 1 {
		t.Fatalf("reordered=%d want >=1", rep.Reordered)
	}
	if rep.TotalLost != 3 || rep.Recovered != 1 {
		t.Fatalf("lost=%d recovered=%d", rep.TotalLost, rep.Recovered)
	}
	if rep.LossRecovery < 0.33 || rep.LossRecovery > 0.34 {
		t.Fatalf("ratio=%v want ~0.333", rep.LossRecovery)
	}
	if len(rep.UnrecoveredGaps) != 1 || rep.UnrecoveredGaps[0].PacketCount != 2 {
		t.Fatalf("gaps=%+v want one gap of 2", rep.UnrecoveredGaps)
	}
	// Recovered payload must byte-match the original.
	for _, rp := range rep.RecoveredPackets {
		if rp.Sequence == 102 && !bytes.Equal(rp.Payload, payloads[102]) {
			t.Fatal("XOR recovery payload mismatch")
		}
	}
}

func TestRTPSequenceWrap(t *testing.T) {
	seqs := []uint16{65534, 65535, 0, 1}
	var pkts []rtp.Packet
	for _, s := range seqs {
		pkts = append(pkts, pkt(s, []byte{1, 2, 3, 4}))
	}
	rep := repair.RepairRTP(pkts, nil)
	if rep.FirstExtended != 65534 || rep.LastExtended != 65537 {
		t.Fatalf("extended range=%d..%d want 65534..65537", rep.FirstExtended, rep.LastExtended)
	}
	if rep.TotalLost != 0 || rep.LossRecovery != 1.0 {
		t.Fatalf("wrap stream should be intact: lost=%d ratio=%v", rep.TotalLost, rep.LossRecovery)
	}
}

func TestTimelineRebaseMonotonicAndAVSync(t *testing.T) {
	// Use a real *testing.T via closure helper instead of the placeholder.
	mk := func(index int, ts uint32, base uint64, dur uint32, disc bool) repair.Segment {
		spec := scan.FragmentSpec{TrackID: 1, Sequence: uint32(index + 1), BaseDecodeTime: base, TFDTv1: true}
		spec.Samples = []scan.SampleSpec{{Duration: dur, Size: 4, CTO: 2}, {Duration: dur, Size: 4, CTO: 2}}
		r, err := scan.ScanReader(bytes.NewReader(scan.BuildFragment(spec)), "tr")
		if err != nil {
			t.Fatal(err)
		}
		return repair.Segment{Index: index, Timescale: ts, Discontinuity: disc, Fragment: r.Fragments[0]}
	}
	video := repair.RepairTrack("v", 90000, []repair.Segment{
		mk(1, 90000, 9_090_000+90000, 45000, false), // delivered out of order
		mk(0, 90000, 9_090_000, 45000, false),       // arbitrary non-zero start
	})
	audio := repair.RepairTrack("a", 48000, []repair.Segment{
		mk(0, 48000, 777_000, 24000, false),
		mk(1, 48000, 777_000+48000, 24000, false),
	})
	if len(video.Ranges) != 2 || video.Ranges[0].StartCommon != 0 || video.Ranges[1].StartCommon != 720000 {
		t.Fatalf("video ranges wrong: %+v", video.Ranges)
	}
	for i := 1; i < len(video.Samples); i++ {
		if video.Samples[i].DTS < video.Samples[i-1].DTS {
			t.Fatal("DTS not monotonic")
		}
		if video.Samples[i].PTS-video.Samples[i].DTS != 16 { // CTO 2 @90k *8
			t.Fatal("PTS-DTS composition relation changed")
		}
	}
	if d := repair.MaxAVSyncMillis(video, audio); d != 0 {
		t.Fatalf("av sync drift=%v want 0", d)
	}
}

func TestTimelineDeclaredDiscontinuityAndAmbiguousIsolation(t *testing.T) {
	mk := func(index int, base uint64, disc bool) repair.Segment {
		spec := scan.FragmentSpec{TrackID: 1, BaseDecodeTime: base, TFDTv1: true,
			Samples: []scan.SampleSpec{{Duration: 45000, Size: 4}, {Duration: 45000, Size: 4}}}
		r, _ := scan.ScanReader(bytes.NewReader(scan.BuildFragment(spec)), "v")
		return repair.Segment{Index: index, Timescale: 90000, Discontinuity: disc, Fragment: r.Fragments[0]}
	}
	declared := repair.RepairTrack("v", 90000, []repair.Segment{
		mk(0, 5000, false), mk(1, 9, true), // producer reset, declared by manifest
	})
	if len(declared.Ranges) != 2 {
		t.Fatalf("declared discontinuity should keep both ranges: %+v", declared.Ranges)
	}

	ambiguous := repair.RepairTrack("v", 90000, []repair.Segment{
		mk(0, 5000, false), mk(1, 9, false), // huge undeclared backward jump
	})
	if len(ambiguous.IsolatedRanges) != 1 || ambiguous.IsolatedRanges[0].SegmentIndex != 1 {
		t.Fatalf("expected segment 1 isolated, got %+v faults=%v", ambiguous.IsolatedRanges, ambiguous.Record.Faults)
	}
	if len(ambiguous.Ranges) != 1 {
		t.Fatal("ambiguous segment must not enter the trustworthy timeline")
	}
}
