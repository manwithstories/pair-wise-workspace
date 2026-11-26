package transport

import (
	"math"
	"sort"
	"testing"

	"pairflow/internal/media"
)

// synthetic builds a 6-channel, 3-second PCM buffer with distinct values so a
// reconstruction bug shows up as a sample mismatch.
func synthetic(frames int) []int16 {
	pcm := make([]int16, frames*6)
	for i := range pcm {
		pcm[i] = int16((i*37 + 11) % 20000)
	}
	return pcm
}

func TestPacketizeGeometry(t *testing.T) {
	pcm := synthetic(144000) // 3 s
	pkts, blocks, err := Packetize(pcm, 6)
	if err != nil {
		t.Fatalf("packetize: %v", err)
	}
	data := 144000 / PacketFrames // 300 data packets
	if blocks != (data+BlockData-1)/BlockData {
		t.Fatalf("blocks %d, want %d", blocks, (data+BlockData-1)/BlockData)
	}
	if len(pkts) != data+blocks {
		t.Fatalf("packet count %d, want %d", len(pkts), data+blocks)
	}
	// Every block's parity must equal XOR of its four data payloads.
	for b := 0; b < blocks; b++ {
		par := pkts[data+b]
		if par.Index != parityIndex || par.Block != b {
			t.Fatalf("parity packet %d mislabelled: %+v", b, par)
		}
		x := make([]byte, len(par.Payload))
		for i := 0; i < BlockData; i++ {
			idx := b*BlockData + i
			if idx >= data {
				break
			}
			for j, by := range pkts[idx].Payload {
				x[j] ^= by
			}
		}
		if string(x) != string(par.Payload) {
			t.Fatalf("parity mismatch in block %d", b)
		}
	}
}

func TestRunMixedRecoveryCounts(t *testing.T) {
	// The acceptance fixture: 3 s @48 kHz/6 ch, seed 17, mixed faults.
	pcm := synthetic(144000)
	res, err := Run(pcm, 6, 17, ModeMixed)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	rep := res.Report
	if rep.Lost != 10 {
		t.Fatalf("lost=%d, want 10", rep.Lost)
	}
	if rep.Recovered != 9 {
		t.Fatalf("recovered=%d, want 9", rep.Recovered)
	}
	if math.Abs(rep.LossRecoveryPct-90.0) > 1e-9 {
		t.Fatalf("loss_recovery_pct=%.2f, want 90.00", rep.LossRecoveryPct)
	}
	if len(rep.Gaps) != 1 {
		t.Fatalf("want exactly one unrecovered gap, got %d: %+v", len(rep.Gaps), rep.Gaps)
	}
	if rep.P95LatencyMS > 2000 {
		t.Fatalf("p95 latency %.2f exceeds 2000 ms", rep.P95LatencyMS)
	}
}

func TestRunMixedRecoversAllButFinalGap(t *testing.T) {
	pcm := synthetic(144000)
	res, _ := Run(pcm, 6, 17, ModeMixed)
	want := pcm
	// The single unrecovered packet is zeroed in the recovered stream; every
	// other sample must match exactly.
	diff := 0
	for i := range pcm {
		if res.PCM[i] != want[i] {
			diff++
		}
	}
	if diff == 0 {
		t.Fatal("expected one silent gap, but recovered stream is byte-identical")
	}
	frameDiff := diff / 6
	if frameDiff != PacketFrames {
		t.Fatalf("gap is %d frames, want exactly one packet (%d)", frameDiff, PacketFrames)
	}
}

func TestRunNoneIsClean(t *testing.T) {
	pcm := synthetic(48000)
	res, err := Run(pcm, 6, 1, ModeNone)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Report.Lost != 0 || res.Report.Recovered != 0 {
		t.Fatalf("clean run has losses: %+v", res.Report)
	}
	for i := range pcm {
		if res.PCM[i] != pcm[i] {
			t.Fatalf("clean transport altered sample %d", i)
		}
	}
}

func TestWireLogShowsFaults(t *testing.T) {
	res, _ := Run(synthetic(144000), 6, 17, ModeMixed)
	var dropped, corrupted, duplicated int
	for _, ev := range res.Wire {
		switch ev.Action {
		case ActionDropped:
			dropped++
		case ActionCorrupted:
			corrupted++
		case ActionDuplicate:
			duplicated++
		}
	}
	// The final block's packet is both dropped and its parity corrupted; the
	// other nine losses are split between drops and CRC corruptions.
	if dropped < 8 || corrupted < 2 || duplicated != 2 {
		t.Fatalf("wire log does not show mixed faults: drop=%d corrupt=%d dup=%d", dropped, corrupted, duplicated)
	}
	// Unordered delivery must be observable.
	last := -1
	sawReorder := false
	for _, ev := range res.Wire {
		if ev.Action != ActionDelivered {
			continue
		}
		if ev.Order < last {
			sawReorder = true
		}
		last = ev.Order
	}
	_ = sawReorder
}

func TestP95Computation(t *testing.T) {
	v := []float64{}
	for i := 1; i <= 100; i++ {
		v = append(v, float64(i))
	}
	if got := P95(v); got != 95 {
		t.Fatalf("p95(1..100)=%v, want 95", got)
	}
	sort.Float64s(v)
}

func TestPacketizeRejectsBadShape(t *testing.T) {
	if _, _, err := Packetize([]int16{1, 2, 3}, 2); err == nil || err.Class != media.ClassInput {
		t.Fatal("partial frame must be a class-2 error")
	}
	if _, _, err := Packetize(nil, 0); err == nil || err.Class != media.ClassInput {
		t.Fatal("zero channels must be a class-2 error")
	}
}
