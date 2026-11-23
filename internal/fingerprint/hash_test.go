package fingerprint

import (
	"testing"

	"medpipe/internal/model"
)

func frame(seq uint32, ts int64, payload, meta []byte) *model.Frame {
	return &model.Frame{Seq: seq, TimestampNS: ts, Payload: payload, Metadata: meta}
}

func TestCanonicalStable(t *testing.T) {
	a := Canonical([]byte{1, 2, 3, 0, 0})
	b := Canonical([]byte{1, 2, 3})
	if string(a) != string(b) {
		t.Fatalf("trailing zero padding must normalize: %v vs %v", a, b)
	}
	if Of([]byte{9, 9}) != Of([]byte{9, 9, 0}) {
		t.Fatal("fingerprints of padded variants must match")
	}
	if Of([]byte{9, 8}) == Of([]byte{9, 9}) {
		t.Fatal("different payloads must differ")
	}
}

func TestDedupReusesReference(t *testing.T) {
	cat := NewCatalog()
	ref1 := cat.Add(frame(0, 0, []byte{7, 7, 0}, []byte(`{"k":1}`)))
	ref2 := cat.Add(frame(1, 40e6, []byte{7, 7}, []byte(`{"k":2}`)))
	if ref1 != ref2 {
		t.Fatalf("equal canonical payloads must share reference: %s vs %s", ref1, ref2)
	}
	if cat.UniqueCount() != 1 {
		t.Fatalf("unique=%d want 1", cat.UniqueCount())
	}
	stats := cat.Stats()
	if stats.TotalFrames != 2 || stats.ReusedRefs != 1 {
		t.Fatalf("stats wrong: %+v", stats)
	}
	e, ok := cat.Entry(ref1[len("ref:"):])
	if !ok || len(e.Seqs) != 2 || e.Seqs[0] != 0 || e.Seqs[1] != 1 {
		t.Fatalf("both timeline positions must be retained: %+v", e)
	}
}

func TestSamePayloadDifferentMetadataKeepsPositions(t *testing.T) {
	cat := NewCatalog()
	f1 := frame(10, 100, []byte{5, 5}, []byte(`{"color":"yuv420"}`))
	f2 := frame(11, 140, []byte{5, 5}, []byte(`{"color":"bt2020"}`))
	r1, r2 := cat.Add(f1), cat.Add(f2)
	if r1 != r2 {
		t.Fatal("payload identity must ignore metadata")
	}
	// Frames themselves are untouched: distinct seq and timestamp survive.
	if f1.Seq == f2.Seq || f1.TimestampNS == f2.TimestampNS {
		t.Fatal("frame timeline positions must not be merged")
	}
}

func TestEntriesFirstSeenOrder(t *testing.T) {
	cat := NewCatalog()
	cat.Add(frame(0, 0, []byte{1}, nil))
	cat.Add(frame(1, 1, []byte{2}, nil))
	cat.Add(frame(2, 2, []byte{1}, nil))
	entries := cat.Entries()
	if len(entries) != 2 || entries[0].Digest != Of([]byte{1}) || entries[1].Digest != Of([]byte{2}) {
		t.Fatalf("ordering unstable: %+v", entries)
	}
}
