package fixture

import (
	"testing"

	"mediarun/internal/ebml"
	"mediarun/internal/transport"
)

func TestSmokeGeneratedPipeline(t *testing.T) {
	data, tracks := BuildEBML(41, 12_000_000_000, 1_800_000)
	if len(tracks) != 3 {
		t.Fatalf("tracks=%d", len(tracks))
	}
	wantBytes := int64(1_800_000) * 12_000_000_000 / 8_000_000_000
	if int64(len(data)) != wantBytes {
		t.Fatalf("bytes=%d want %d", len(data), wantBytes)
	}
	doc, err := ebml.Parse(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if doc.TimeScale != 1_000_000 {
		t.Fatalf("timescale=%d", doc.TimeScale)
	}
	// 24 clusters: 24 video + 48 audio + 12 subtitle blocks.
	if len(doc.Blocks) != 84 {
		t.Fatalf("blocks=%d", len(doc.Blocks))
	}
	pkts := Packetize(data, RTPOptions{Duplicates: true, Reorder: true, WrapStart: true})
	res := transport.Process(pkts)
	if res.Stats.Lost != 0 || res.Stats.Duplicates == 0 {
		t.Fatalf("stats=%+v", res.Stats)
	}
	joined := transport.PayloadBytes(res)
	if int64(len(joined)) != wantBytes {
		t.Fatalf("reassembled=%d", len(joined))
	}
	for i := range joined {
		if joined[i] != data[i] {
			t.Fatalf("reassembly mismatch at %d", i)
		}
	}
}

func TestCorruptedRejected(t *testing.T) {
	_, err := ebml.Parse(Corrupted())
	ee, ok := err.(*ebml.Error)
	if !ok {
		t.Fatalf("err=%v", err)
	}
	if ee.Code != ebml.ErrTruncatedVINT {
		t.Fatalf("code=%s", ee.Code)
	}
}
