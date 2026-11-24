package recovery_test

import (
	"bytes"
	"testing"

	"relay/internal/demux"
	"relay/internal/fixture"
	"relay/internal/media"
	"relay/internal/recovery"
)

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

func recoverSpec(t *testing.T, imp fixture.RTPImpairments) *recovery.Result {
	t.Helper()
	spec := fixture.Spec{
		SourceID: 5, Kind: media.KindVideo, TimescaleHz: 24, ClockRateHz: 90000,
		Width: 640, Height: 360, Samples: 24, StartRefNS: 1000, PayloadSeed: 55,
	}
	data := fixture.RTPLog(spec, imp)
	recs, hdr, err := demux.ReadRTPLog(bytesReader(data))
	if err != nil {
		t.Fatal(err)
	}
	res, err := recovery.Recover(recs, hdr)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestSingleLossRecoveredByXOR(t *testing.T) {
	res := recoverSpec(t, fixture.RTPImpairments{SingleLoss: []uint16{9}})
	if res.Recovered != 1 {
		t.Fatalf("recovered: got %d want 1", res.Recovered)
	}
	if len(res.Gaps) != 0 {
		t.Fatalf("unexpected gaps: %+v", res.Gaps)
	}
	if res.MediaCount != 24 {
		t.Fatalf("media count: got %d want 24", res.MediaCount)
	}
}

func TestDuplicateRemoved(t *testing.T) {
	res := recoverSpec(t, fixture.RTPImpairments{DuplicateAt: []uint16{3, 11}})
	if res.Duplicates != 2 {
		t.Fatalf("duplicates: got %d want 2", res.Duplicates)
	}
	if res.MediaCount != 24 {
		t.Fatalf("dedup media count: got %d want 24", res.MediaCount)
	}
}

func TestReorderProducesInOrderOutput(t *testing.T) {
	res := recoverSpec(t, fixture.RTPImpairments{Reorder: true})
	for i := 1; i < len(res.Frames); i++ {
		if res.Frames[i].Meta.Sequence <= res.Frames[i-1].Meta.Sequence {
			t.Fatalf("output not ordered at %d", i)
		}
	}
}

func TestDoubleLossMarkedAsGap(t *testing.T) {
	// Drop seq 16 and 17 inside one group: XOR cannot solve two unknowns.
	res := recoverSpec(t, fixture.RTPImpairments{DoubleLossAt: []uint16{16}})
	if res.Recovered != 0 {
		t.Fatalf("no packet should be reconstructed, got %d", res.Recovered)
	}
	if len(res.Gaps) != 1 {
		t.Fatalf("gaps: got %d want 1", len(res.Gaps))
	}
	g := res.Gaps[0]
	if g.FirstSequence != 16 || g.LastSequence != 17 {
		t.Fatalf("gap range wrong: %+v", g)
	}
}

func TestRecoveryPctClean(t *testing.T) {
	res := recoverSpec(t, fixture.RTPImpairments{})
	if pct := 100.0; float64(res.MediaCount)*100/float64(res.ExpectedSpan) != pct {
		t.Fatalf("clean recovery should be 100%%, got %d/%d", res.MediaCount, res.ExpectedSpan)
	}
}

func TestEncodeRepairRoundtrip(t *testing.T) {
	members := [][]byte{{1, 2, 3, 4}, {5, 6, 7}, {9, 9, 9, 9, 9}}
	rp := recovery.EncodeRepair(8, members)
	base, count, xored, err := recovery.DecodeRepair(rp)
	if err != nil || base != 8 || count != 3 {
		t.Fatalf("repair decode: base=%d count=%d err=%v", base, count, err)
	}
	// XOR all members again to confirm the stored parity.
	want := make([]byte, 5)
	for _, m := range members {
		for i, b := range m {
			want[i] ^= b
		}
	}
	if string(xored) != string(want) {
		t.Fatalf("parity mismatch: % x want % x", xored, want)
	}
}
