package verify

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"mediadeliver/internal/model"
)

func sourceSet() (map[string][]*model.Packet, []*model.Packet) {
	t1 := []*model.Packet{
		{SourceID: "cam1", TrackID: 1, Seq: 0, Payload: []byte("aaaa")},
		{SourceID: "cam1", TrackID: 1, Seq: 1, Payload: []byte("bbbb")},
		{SourceID: "cam1", TrackID: 1, Seq: 2, Payload: []byte("cccc")},
	}
	t2 := []*model.Packet{
		{SourceID: "cam1", TrackID: 2, Seq: 0, Payload: []byte("dd")},
		{SourceID: "cam1", TrackID: 2, Seq: 1, Payload: []byte("ee")},
	}
	src := map[string][]*model.Packet{"cam1:1": t1, "cam1:2": t2}
	all := append(append([]*model.Packet{}, t1...), t2...)
	return src, all
}

func buildRecord(t *testing.T) (*model.SegmentRecord, []byte) {
	src, all := sourceSet()
	h := sha256.New()
	var out bytes.Buffer
	for _, p := range all {
		out.Write(p.Payload)
		h.Write(p.Payload)
	}
	rec := &model.SegmentRecord{
		Index: 0, OutputFile: "seg0",
		Ranges: []model.SourceRange{
			{MaterialID: "cam1", TrackID: 1, FirstSeq: 0, LastSeq: 2, Packets: 3, Timescale: 90000},
			{MaterialID: "cam1", TrackID: 2, FirstSeq: 0, LastSeq: 1, Packets: 2, Timescale: 48000},
		},
		PacketCount: 5,
		SHA256Hex:   hex.EncodeToString(h.Sum(nil)),
		Bytes:       int64(out.Len()),
	}
	_ = src
	return rec, out.Bytes()
}

func loaderFor(src map[string][]*model.Packet) SourcePacketFunc {
	return func(materialID string, trackID uint32) ([]*model.Packet, error) {
		return src[materialID+":"+itoa(trackID)], nil
	}
}

func TestVerifySegmentPasses(t *testing.T) {
	rec, out := buildRecord(t)
	if err := VerifySegment(rec, bytes.NewReader(out)); err != nil {
		t.Fatalf("valid segment should verify: %v", err)
	}
}

func TestVerifySegmentDetectsTamper(t *testing.T) {
	rec, out := buildRecord(t)
	tampered := append([]byte(nil), out...)
	tampered[0] ^= 0xff
	err := VerifySegment(rec, bytes.NewReader(tampered))
	if model.KindOf(err) != model.KindConflict {
		t.Fatalf("tampered output must be KindConflict, got %v", err)
	}
}

func TestVerifyProvenanceRebuildsDigest(t *testing.T) {
	src, _ := sourceSet()
	rec, _ := buildRecord(t)
	if err := VerifyProvenance(rec, loaderFor(src)); err != nil {
		t.Fatalf("provenance reconstruction failed: %v", err)
	}
}

func TestVerifyProvenanceDetectsRangeMismatch(t *testing.T) {
	src, _ := sourceSet()
	rec, _ := buildRecord(t)
	// Lie about the packet count in one range.
	rec.Ranges[0].Packets = 99
	err := VerifyProvenance(rec, loaderFor(src))
	if model.KindOf(err) != model.KindConflict {
		t.Fatalf("range coverage lie must be KindConflict, got %v", err)
	}
}

func TestRejectsBadDigestLength(t *testing.T) {
	rec, out := buildRecord(t)
	rec.SHA256Hex = "abcd"
	if err := VerifySegment(rec, bytes.NewReader(out)); model.KindOf(err) != model.KindConflict {
		t.Fatalf("short digest must be KindConflict, got %v", err)
	}
}

func TestHashReaderDeterministic(t *testing.T) {
	data := []byte("reproducible media payload")
	h1, n1, err := HashReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	h2, n2, _ := HashReader(bytes.NewReader(data))
	if h1 != h2 || n1 != n2 || n1 != int64(len(data)) || len(h1) != 64 {
		t.Fatalf("hash must be stable 64-hex: %s %d", h1, n1)
	}
}

func itoa(v uint32) string {
	if v == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
