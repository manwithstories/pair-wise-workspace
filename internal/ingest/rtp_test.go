package ingest_test

import (
	"testing"

	"mediad/internal/ingest"
)

func TestReorderRestoresSequence(t *testing.T) {
	// Packets 1..10 arrive shuffled; each pair is one frame.
	var pkts []ingest.PreviewPacket
	for i := uint16(0); i < 10; i++ {
		pkts = append(pkts, ingest.PreviewPacket{Seq: i + 1, FrameNo: uint64(i/2) + 1, Marker: i%2 == 1})
	}
	// Shuffle deterministically.
	order := []int{3, 0, 9, 1, 8, 2, 7, 4, 6, 5}
	shuffled := make([]ingest.PreviewPacket, 0, len(pkts))
	for _, i := range order {
		shuffled = append(shuffled, pkts[i])
	}
	st, err := ingest.Reorder(shuffled)
	if err != nil {
		t.Fatal(err)
	}
	if st.Received != 10 || st.Lost != 0 || st.Duplicates != 0 {
		t.Fatalf("stats %+v", st)
	}
	if len(st.Frames) != 5 {
		t.Fatalf("frames = %v, want 5 complete", st.Frames)
	}
	if st.Reordered == 0 {
		t.Fatal("expected some reordering to be reported")
	}
}

func TestReorderReportsLossAndGap(t *testing.T) {
	var pkts []ingest.PreviewPacket
	for seq := uint16(1); seq <= 10; seq++ {
		if seq == 5 || seq == 6 {
			continue // two packets lost
		}
		pkts = append(pkts, ingest.PreviewPacket{Seq: seq, FrameNo: uint64(seq/2) + 1, Marker: seq%2 == 0})
	}
	st, _ := ingest.Reorder(pkts)
	if st.Lost != 2 {
		t.Fatalf("lost = %d, want 2", st.Lost)
	}
	if len(st.Gaps) != 1 || st.Gaps[0].FromSeq != 5 || st.Gaps[0].ToSeq != 6 {
		t.Fatalf("gaps = %+v", st.Gaps)
	}
}

func TestReorderCountsDuplicates(t *testing.T) {
	pkts := []ingest.PreviewPacket{
		{Seq: 1, FrameNo: 1}, {Seq: 2, FrameNo: 1, Marker: true}, {Seq: 2, FrameNo: 1, Marker: true},
	}
	st, _ := ingest.Reorder(pkts)
	if st.Duplicates != 1 {
		t.Fatalf("duplicates = %d, want 1", st.Duplicates)
	}
}

func TestReorderHandlesSequenceWrap(t *testing.T) {
	// Sequences approach 65535 and wrap to 0; this must not read as a huge gap.
	pkts := []ingest.PreviewPacket{
		{Seq: 65534, FrameNo: 100},
		{Seq: 65535, FrameNo: 100, Marker: true},
		{Seq: 0, FrameNo: 101},
		{Seq: 1, FrameNo: 101, Marker: true},
	}
	st, _ := ingest.Reorder(pkts)
	if st.Lost != 0 {
		t.Fatalf("lost = %d, want 0 across wrap (gaps %+v)", st.Lost, st.Gaps)
	}
	if len(st.Frames) != 2 {
		t.Fatalf("frames = %v", st.Frames)
	}
}

func TestEncodeDecodePreviewRoundTrip(t *testing.T) {
	in := ingest.PreviewPacket{Seq: 42, FrameNo: 7, Marker: true, Payload: []byte("fragment")}
	buf, err := ingest.EncodePreview(in, 0xdeadbeef)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ingest.DecodePreview(buf)
	if err != nil {
		t.Fatal(err)
	}
	if out.Seq != in.Seq || out.FrameNo != in.FrameNo || out.Marker != in.Marker {
		t.Fatalf("decoded %+v, want %+v", out, in)
	}
	if string(out.Payload) != "fragment" {
		t.Fatalf("payload = %q", out.Payload)
	}
}

func TestMissingFramesPreservesGap(t *testing.T) {
	st := &ingest.RTPStats{Frames: []uint64{1, 2, 5, 6}}
	missing := ingest.MissingFrames(st)
	if len(missing) != 2 || missing[0] != 3 || missing[1] != 4 {
		t.Fatalf("missing = %v, want [3 4]", missing)
	}
}
