package stream

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// box builds one MP4 box: 4-byte size, 4-byte type, optional payload.
func box(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b, uint32(8+len(payload)))
	copy(b[4:], typ)
	copy(b[8:], payload)
	return b
}

// stssBox builds an stss box containing the given 1-based sample numbers.
func stssBox(samples []uint32) []byte {
	payload := make([]byte, 8+4*len(samples)) // fullbox header + count + entries
	binary.BigEndian.PutUint32(payload[4:], uint32(len(samples)))
	for i, s := range samples {
		binary.BigEndian.PutUint32(payload[8+i*4:], s)
	}
	return box("stss", payload)
}

// minimalMP4 builds a structurally valid MP4 with ftyp, a moov/stbl/stss
// index and an mdat payload. Keyframes are placed every 25 samples.
func minimalMP4() []byte {
	var out []byte
	out = append(out, box("ftyp", []byte("isom\x00\x00\x00\x00isom"))...)
	stbl := append(stssBox([]uint32{1, 26, 51, 76}), box("stsz", make([]byte, 12))...)
	mdia := box("minf", box("stbl", stbl))
	moov := box("trak", box("mdia", mdia))
	out = append(out, box("moov", moov)...)
	out = append(out, box("mdat", make([]byte, 128))...)
	return out
}

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAnalyzeMP4Healthy(t *testing.T) {
	p := writeTemp(t, "ok.mp4", minimalMP4())
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := AnalyzeMP4(f, p)
	if err != nil {
		t.Fatal(err)
	}
	if st.DamageOffset != -1 || len(st.Faults) != 0 {
		t.Fatalf("healthy file must have no damage: %+v", st)
	}
	if len(st.Boxes) < 6 {
		t.Fatalf("expected nested box walk, got %d boxes", len(st.Boxes))
	}
	if st.KeyframeCount != 4 || len(st.KeyframeGaps) != 3 {
		t.Fatalf("keyframes wrong: count=%d gaps=%d", st.KeyframeCount, len(st.KeyframeGaps))
	}
	for _, g := range st.KeyframeGaps {
		if g.Distance != 25 {
			t.Fatalf("keyframe gap distance = %d, want 25", g.Distance)
		}
	}
}

func TestAnalyzeMP4TruncatedTail(t *testing.T) {
	full := minimalMP4()
	// Locate the mdat box header and cut inside its payload.
	mdatOffset := int64(-1)
	for i := 0; i+8 <= len(full); i++ {
		if string(full[i+4:i+8]) == "mdat" {
			mdatOffset = int64(i)
			break
		}
	}
	if mdatOffset < 0 {
		t.Fatal("mdat not found")
	}
	declaredSize := int64(binary.BigEndian.Uint32(full[mdatOffset:]))
	// Cut inside the mdat payload: only a few of its declared bytes remain.
	// The truncated file length is itself the first unreadable offset.
	cutAt := mdatOffset + 16
	truncated := append([]byte(nil), full[:cutAt]...)
	firstBad := int64(cutAt)
	if firstBad >= int64(len(full)) {
		t.Fatalf("test setup: cut %d must be inside original file %d", firstBad, len(full))
	}

	p := writeTemp(t, "trunc.mp4", truncated)
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := AnalyzeMP4(f, p)
	if err != nil {
		t.Fatal(err)
	}
	if st.DamageOffset < 0 {
		t.Fatal("truncated mdat must set damage offset")
	}
	// Required accuracy: within 1 byte of the first unreadable offset.
	diff := st.DamageOffset - firstBad
	if diff < -1 || diff > 1 {
		t.Fatalf("damage offset %d, want %d (±1)", st.DamageOffset, firstBad)
	}
	// Boxes before the damage must still be fully usable.
	if st.KeyframeCount != 4 {
		t.Fatalf("preceding index lost after tail truncation: %d", st.KeyframeCount)
	}
	if declaredSize <= 0 {
		t.Fatal("declared size sanity")
	}
}

func TestAnalyzeMP4TruncatedHeader(t *testing.T) {
	full := minimalMP4()
	// Cut with only 3 bytes of what should be the next 8-byte box header.
	mdat := int64(-1)
	for i := 0; i+8 <= len(full); i++ {
		if string(full[i+4:i+8]) == "mdat" {
			mdat = int64(i)
			break
		}
	}
	cut := append([]byte(nil), full[:mdat+3]...)
	p := writeTemp(t, "hdr.mp4", cut)
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := AnalyzeMP4(f, p)
	if err != nil {
		t.Fatal(err)
	}
	if st.DamageOffset != mdat {
		t.Fatalf("header truncation offset = %d, want %d", st.DamageOffset, mdat)
	}
}

func TestAnalyzeMP4IndexRebuild(t *testing.T) {
	// moov without any stss: index must be flagged for reconstruction.
	var out []byte
	out = append(out, box("ftyp", []byte("isom"))...)
	out = append(out, box("moov", box("trak", box("mdia", box("minf", box("stbl", box("stsz", make([]byte, 12)))))))...)
	out = append(out, box("mdat", make([]byte, 64))...)
	p := writeTemp(t, "noindex.mp4", out)
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := AnalyzeMP4(f, p)
	if err != nil {
		t.Fatal(err)
	}
	if !st.RebuiltIndex {
		t.Fatal("missing stss must trigger index rebuild flag")
	}
}
