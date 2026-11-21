package synth

import "testing"

func TestDeterministic(t *testing.T) {
	cfg := Config{Seed: 41, VideoPackets: 300, AudioPackets: 200, LossPercent: 5,
		ReorderPercent: 5, DuplicatePct: 5, WrapAt: 4294967000, DriftPPM: 180, VFR: true}
	a, ta := NewBuilder(cfg).Build()
	b, tb := NewBuilder(cfg).Build()
	if len(a) != len(b) || len(ta.Frames) != len(tb.Frames) {
		t.Fatalf("length mismatch: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].SSRC != b[i].SSRC || a[i].Seq != b[i].Seq || a[i].Timestamp != b[i].Timestamp {
			t.Fatalf("non-deterministic record at %d", i)
		}
	}
}

func TestLossReorderDuplicate(t *testing.T) {
	cfg := Config{Seed: 1, VideoPackets: 400, AudioPackets: 200,
		LossPercent: 20, ReorderPercent: 30, DuplicatePct: 20}
	recs, truth := NewBuilder(cfg).Build()
	rtpCount, dupCount := 0, 0
	seen := map[[2]uint32]int{}
	for _, r := range recs {
		if r.Kind != 0 {
			continue
		}
		rtpCount++
		key := [2]uint32{r.SSRC, uint32(r.Seq)}
		seen[key]++
		if seen[key] == 2 {
			dupCount++
		}
	}
	if rtpCount == 0 || dupCount == 0 {
		t.Fatalf("expected traffic and duplicates, got rtp=%d dup=%d", rtpCount, dupCount)
	}
	// Loss: recovered distinct sequence count is below ideal generation.
	ideal := cfg.VideoPackets + cfg.AudioPackets
	distinct := len(seen)
	if distinct >= ideal {
		t.Fatalf("expected loss, distinct=%d ideal=%d", distinct, ideal)
	}
	if len(truth.KeyframeIDs) == 0 || len(truth.Frames) == 0 {
		t.Fatal("ground truth must label frames and keyframes")
	}
}

func TestWrap(t *testing.T) {
	cfg := Config{Seed: 7, VideoPackets: 400, WrapAt: 6000}
	recs, _ := NewBuilder(cfg).Build()
	for _, r := range recs {
		if r.Kind == 0 && uint64(r.Timestamp) >= cfg.WrapAt {
			t.Fatalf("timestamp %d exceeds wrap point %d", r.Timestamp, cfg.WrapAt)
		}
	}
}

func TestDrift(t *testing.T) {
	plain, _ := NewBuilder(Config{Seed: 7, VideoPackets: 200}).Build()
	drifted, _ := NewBuilder(Config{Seed: 7, VideoPackets: 200, DriftPPM: 5000}).Build()
	var pLast, dLast uint32
	for _, r := range plain {
		if r.SSRC == VideoSSRC {
			pLast = r.Timestamp
		}
	}
	for _, r := range drifted {
		if r.SSRC == VideoSSRC {
			dLast = r.Timestamp
		}
	}
	if dLast <= pLast {
		t.Fatalf("drifting clock must advance further: %d vs %d", dLast, pLast)
	}
}

func TestVFR(t *testing.T) {
	cfr, _ := NewBuilder(Config{Seed: 3, VideoPackets: 300}).Build()
	vfr, _ := NewBuilder(Config{Seed: 3, VideoPackets: 300, VFR: true}).Build()
	steps := func(recs []Record) map[uint32]bool {
		last := map[uint32]uint32{}
		steps := map[uint32]bool{}
		for _, r := range recs {
			if r.Kind != 0 || r.SSRC != VideoSSRC {
				continue
			}
			if l, ok := last[r.SSRC]; ok && r.Timestamp != l {
				steps[r.Timestamp-l] = true
			}
			last[r.SSRC] = r.Timestamp
		}
		return steps
	}
	if len(steps(vfr)) <= len(steps(cfr)) {
		t.Fatalf("VFR must expose more cadence steps: vfr=%d cfr=%d", len(steps(vfr)), len(steps(cfr)))
	}
}

func TestTruncate(t *testing.T) {
	recs, _ := NewBuilder(Config{Seed: 5, VideoPackets: 100, AudioPackets: 50}).Build()
	short := Truncate(recs, 10)
	if len(short) != 10 {
		t.Fatalf("truncate: got %d", len(short))
	}
}

func TestRTCPNeverDropped(t *testing.T) {
	cfg := Config{Seed: 9, VideoPackets: 400, LossPercent: 60, ReorderPercent: 40}
	recs, _ := NewBuilder(cfg).Build()
	sr := 0
	for _, r := range recs {
		if r.Kind == 1 {
			sr++
		}
	}
	if sr == 0 {
		t.Fatal("RTCP sender reports must survive degradation")
	}
}
