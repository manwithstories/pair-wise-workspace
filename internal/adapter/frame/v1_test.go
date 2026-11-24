package frame

import (
	"testing"

	"media-check/internal/model"
)

func genAndPack(t *testing.T, n int) ([]Record, [][]byte) {
	t.Helper()
	recs := Generate(GenOptions{
		Count: n, StartSeq: 100, StartTS: 5000, TSDelta: 100,
		GOPSize: 10, Width: 640, Height: 360, PayloadSize: 32, Seed: 7,
		Color: model.ColorMeta{Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"},
	})
	pkts, err := ToPackets(recs, 99, 1)
	if err != nil {
		t.Fatal(err)
	}
	return recs, pkts
}

func segOf(seq uint32) int { return int((seq - 100) / 10) }

func TestIngestClean(t *testing.T) {
	_, pkts := genAndPack(t, 30)
	res := Adapter{}.Ingest(pkts, segOf)
	if len(res.Faults) != 0 {
		t.Fatalf("clean input produced faults: %v", res.Faults)
	}
	if len(res.Frames) != 30 {
		t.Fatalf("got %d frames", len(res.Frames))
	}
	for i, f := range res.Frames {
		if f.Time != int64(5000+i*100) {
			t.Fatalf("frame %d time %d", i, f.Time)
		}
	}
}

func TestIngestHashCorruptLocatesFirstSegment(t *testing.T) {
	recs, _ := genAndPack(t, 30)
	// corrupt two frames in the second segment (seq 110..119)
	recs[12].Payload[0] ^= 0xFF
	recs[15].Payload[1] ^= 0xFF
	pkts, err := ToPackets(recs, 99, 1)
	if err != nil {
		t.Fatal(err)
	}
	res := Adapter{}.Ingest(pkts, segOf)
	if !res.Faults.HasCode(model.FaultHashMismatch) {
		t.Fatal("expected hash_mismatch fault")
	}
	segs := res.Faults.Segments()
	if len(segs) != 1 || segs[0] != 1 {
		t.Fatalf("faults must locate to first anomalous segment 1, got %v", segs)
	}
	if len(res.Frames) != 28 {
		t.Fatalf("corrupt frames must be excluded, got %d", len(res.Frames))
	}
}

func TestIngestDuplicateAndGap(t *testing.T) {
	recs, _ := genAndPack(t, 20)
	dup := append([]Record{}, recs[:5]...)
	dup = append(dup, recs[4]) // duplicate seq=104
	dup = append(dup, recs[6:]...)
	pkts, err := ToPackets(dup, 99, 1)
	if err != nil {
		t.Fatal(err)
	}
	res := Adapter{}.Ingest(pkts, segOf)
	if !res.Faults.HasCode(model.FaultSeqDuplicate) {
		t.Fatal("expected seq_duplicate")
	}

	gap := append([]Record{}, recs[:5]...)
	gap = append(gap, recs[7:]...) // seq 105,106 missing
	pkts, err = ToPackets(gap, 99, 1)
	if err != nil {
		t.Fatal(err)
	}
	res = Adapter{}.Ingest(pkts, segOf)
	if !res.Faults.HasCode(model.FaultSeqGap) {
		t.Fatal("expected seq_gap")
	}
}

func TestIngestTimestampWrap(t *testing.T) {
	recs := Generate(GenOptions{
		Count: 20, StartSeq: 100, StartTS: 0xFFFFFF00, TSDelta: 100,
		GOPSize: 10, Width: 640, Height: 360, PayloadSize: 32, Seed: 3,
		Color: model.ColorMeta{Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"},
	})
	pkts, err := ToPackets(recs, 99, 1)
	if err != nil {
		t.Fatal(err)
	}
	res := Adapter{}.Ingest(pkts, segOf)
	if len(res.Faults) != 0 {
		t.Fatalf("wrap is normal delivery, faults: %v", res.Faults)
	}
	for i := 1; i < len(res.Frames); i++ {
		if res.Frames[i].Time-res.Frames[i-1].Time != 100 {
			t.Fatalf("non-monotonic unwrap at %d", i)
		}
	}
}

func TestRecordRoundtrip(t *testing.T) {
	recs, _ := genAndPack(t, 3)
	for _, r := range recs {
		got, err := ParseRecord(r.Marshal())
		if err != nil {
			t.Fatal(err)
		}
		if got.Seq != r.Seq || got.CaptureTS != r.CaptureTS || got.Color != r.Color ||
			got.Sum != r.Sum || len(got.Payload) != len(r.Payload) {
			t.Fatalf("record roundtrip mismatch: %+v vs %+v", got, r)
		}
	}
}
