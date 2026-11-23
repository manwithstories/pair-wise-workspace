package mxf

import (
	"errors"
	"testing"

	"mediasync/internal/fixtures"
	"mediasync/internal/model"
)

func TestParseKLVFraming(t *testing.T) {
	b, err := fixtures.BuildMXF(fixtures.DefaultMxfOptions())
	if err != nil {
		t.Fatal(err)
	}
	r := NewReader(b.Data)
	packets, parts, err := r.ParseKLV()
	if err != nil {
		t.Fatalf("ParseKLV: %v", err)
	}
	if len(parts) != 3 {
		t.Fatalf("partitions = %d, want header/body/footer", len(parts))
	}
	if parts[0].Kind != HeaderPartition || parts[2].Kind != FooterPartition {
		t.Fatalf("partition kinds = %s..%s", parts[0].Kind, parts[2].Kind)
	}
	if parts[1].Offset <= parts[0].Offset {
		t.Fatal("body partition should follow header")
	}
	var essence int
	for _, k := range packets {
		if k.Key == essenceUL {
			essence++
			if k.Length <= 0 {
				t.Fatal("essence packet has non-positive length")
			}
			if int64(len(k.Value)) != k.Length {
				t.Fatal("value length disagrees with KLV length")
			}
		}
	}
	want := len(b.Stats[0].Sizes) + len(b.Stats[1].Sizes)
	if essence != want {
		t.Fatalf("essence packets = %d, want %d", essence, want)
	}
}

func TestReadTracksAndHashes(t *testing.T) {
	b, err := fixtures.BuildMXF(fixtures.DefaultMxfOptions())
	if err != nil {
		t.Fatal(err)
	}
	in, err := NewReader(b.Data).Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(in.Tracks) != 2 {
		t.Fatalf("tracks = %d", len(in.Tracks))
	}
	byID := map[uint32]*model.Track{}
	for _, tr := range in.Tracks {
		byID[tr.ID] = tr
	}
	vid := byID[1]
	if vid == nil || vid.Kind != model.KindVideo {
		t.Fatal("video track missing")
	}
	if vid.Timescale != 30000 {
		t.Fatalf("video timescale = %d", vid.Timescale)
	}
	if len(vid.Samples) != len(b.Stats[0].Sizes) {
		t.Fatalf("video samples = %d", len(vid.Samples))
	}
	// Coded bytes at reported offsets must equal the fixture payloads.
	for i, s := range vid.Samples {
		got := b.Data[s.Offset : s.Offset+int64(s.Size)]
		if string(got) != string(b.Stats[0].Payloads[i]) {
			t.Fatalf("video sample %d mismatch at offset %d", i, s.Offset)
		}
	}
	aud := byID[2]
	if aud == nil || aud.Kind != model.KindAudio {
		t.Fatal("audio track missing")
	}
	if aud.Timescale != 48000 {
		t.Fatalf("audio timescale = %d", aud.Timescale)
	}
	for _, st := range in.Stats {
		switch st.TrackID {
		case 1:
			if st.SHA256 != b.Stats[0].Hash {
				t.Fatal("video hash mismatch")
			}
		case 2:
			if st.SHA256 != b.Stats[1].Hash {
				t.Fatal("audio hash mismatch")
			}
		}
	}
}

func TestInvalidKLVLengthRejected(t *testing.T) {
	b, err := fixtures.BuildMXFInvalidKLV(fixtures.DefaultMxfOptions())
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = NewReader(b.Data).ParseKLV()
	if err == nil {
		t.Fatal("expected invalid KLV error")
	}
	var ce *model.CodedError
	if !errors.As(err, &ce) || ce.Code != model.ErrInvalidKLV {
		t.Fatalf("code = %s", model.CodeOf(err))
	}
	if ce.Offset == 0 {
		t.Fatal("offset should be reported")
	}
}

func TestTruncatedMXFRejected(t *testing.T) {
	b, err := fixtures.BuildMXFTruncated(fixtures.DefaultMxfOptions())
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewReader(b.Data).Read()
	if err == nil {
		t.Fatal("expected truncation error")
	}
	if model.CodeOf(err) != model.ErrInvalidKLV {
		t.Fatalf("code = %s", model.CodeOf(err))
	}
}

func TestBERLength(t *testing.T) {
	cases := []struct{ n, want int }{{0, 1}, {127, 1}, {128, 2}, {255, 2}, {256, 3}, {70000, 4}}
	for _, c := range cases {
		enc := fixtures.BERLength(c.n)
		if len(enc) != c.want {
			t.Fatalf("length %d encoded in %d bytes, want %d", c.n, len(enc), c.want)
		}
		got, used, err := readLength(enc, 0)
		if err != nil {
			t.Fatalf("readLength(%d): %v", c.n, err)
		}
		if got != uint64(c.n) || used != len(enc) {
			t.Fatalf("round trip %d -> %d (%d bytes)", c.n, got, used)
		}
	}
}
