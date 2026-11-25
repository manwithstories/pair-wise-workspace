package ebml_test

import (
	"bytes"
	"errors"
	"testing"

	"avcheck/internal/ebml"
	"avcheck/internal/fixture"
	"avcheck/internal/media"
)

// truncateInsideClusterSize cuts the file three bytes into the first
// cluster's size vint.
func truncateInsideClusterSize(t *testing.T, data []byte) []byte {
	t.Helper()
	off := bytes.Index(data, []byte{0x1F, 0x43, 0xB6, 0x75})
	if off < 0 {
		t.Fatal("cluster not found")
	}
	return data[:off+4+3]
}

func TestTruncatedVintFaultOffset(t *testing.T) {
	s, err := fixture.Build(fixture.DefaultSpec(731))
	if err != nil {
		t.Fatal(err)
	}
	data := truncateInsideClusterSize(t, s.Bytes)
	doc, err := ebml.Parse(bytes.NewReader(data), ebml.DefaultLimits())
	if err == nil {
		t.Fatal("expected an error for a cut vint")
	}
	var fs *media.Faults
	if !errors.As(err, &fs) {
		t.Fatalf("error type = %T, want *media.Faults", err)
	}
	if !fs.Has(media.CodeTruncatedVint) {
		t.Fatalf("faults = %v, want truncated_vint", fs)
	}
	f, _ := fs.First(media.CodeTruncatedVint)
	// The vint starts three bytes before the cut.
	cut := int64(len(data))
	if f.Offset != cut-3 {
		t.Fatalf("fault offset = %d, want %d", f.Offset, cut-3)
	}
	if !doc.Truncated() {
		t.Fatal("document not marked truncated")
	}
	if doc.Faults.MaxLevel() < media.LevelFatal {
		t.Fatalf("level = %v, want at least fatal", doc.Faults.MaxLevel())
	}
}

func TestOversizeElementRejectedWithoutAllocation(t *testing.T) {
	s, _ := fixture.Build(fixture.DefaultSpec(1))
	data := append([]byte(nil), s.Bytes...)
	off := bytes.Index(data, []byte{0x1F, 0x43, 0xB6, 0x75})
	// Claim 2^40 bytes in the cluster size vint.
	data[off+4] = 0x01
	for i := off + 5; i < off+12; i++ {
		data[i] = 0x00
	}
	data[off+5] = 0x00
	data[off+6] = 0x00
	data[off+7] = 0x01
	data[off+8] = 0x00
	lim := ebml.DefaultLimits()
	doc, err := ebml.Parse(bytes.NewReader(data), lim)
	if err == nil {
		t.Fatal("expected oversize rejection")
	}
	if !doc.Faults.Has(media.CodeElementTooLarge) {
		t.Fatalf("faults = %v, want element_too_large", doc.Faults)
	}
	f, _ := doc.Faults.First(media.CodeElementTooLarge)
	if f.Offset != int64(off) {
		t.Fatalf("oversize offset = %d, want %d", f.Offset, off)
	}
}

func TestLimitsRejectAboveHardCap(t *testing.T) {
	lim := ebml.DefaultLimits()
	lim.MaxElementSize = ebml.HardMaxElementSize + 1
	if err := lim.Validate(); err == nil {
		t.Fatal("expected rejection above the hard element cap")
	}
	lim = ebml.DefaultLimits()
	lim.MaxTracks = ebml.HardMaxTracks + 1
	if err := lim.Validate(); err == nil {
		t.Fatal("expected rejection above the hard track cap")
	}
}

func TestUnknownLengthSegmentParses(t *testing.T) {
	s, _ := fixture.Build(fixture.DefaultSpec(5))
	// Rewrite the segment size vint to the 8-byte "unknown" value.
	seg := []byte{0x18, 0x53, 0x80, 0x67}
	off := bytes.Index(s.Bytes, seg)
	if off < 0 {
		t.Fatal("segment not found")
	}
	data := append([]byte(nil), s.Bytes...)
	// The fixture always writes an 8-byte size vint after the segment ID;
	// an 8-byte unknown-length vint is 0x01 followed by ten... seven 0xFF.
	data[off+4] = 0x01
	for i := 1; i < 8; i++ {
		data[off+4+i] = 0xFF
	}
	doc, err := ebml.Parse(bytes.NewReader(data), ebml.DefaultLimits())
	if err != nil {
		t.Fatalf("unknown-length segment: %v (%v)", err, doc.Faults)
	}
	if len(doc.Blocks) != len(s.Blocks) {
		t.Fatalf("blocks = %d, want %d", len(doc.Blocks), len(s.Blocks))
	}
}

func TestUnknownTrackIsFatalNotAbort(t *testing.T) {
	spec := fixture.DefaultSpec(3)
	// A block for track 7, which is never declared.
	s, err := fixture.BuildWithExtraBlock(spec, 7, 100_000_000, []byte("stray"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ebml.Parse(bytes.NewReader(s.Bytes), ebml.DefaultLimits())
	if err != nil {
		t.Fatalf("parse: %v (%v)", err, doc.Faults)
	}
	if !doc.Faults.Has(media.CodeUnknownTrack) {
		t.Fatalf("faults = %v, want unknown_track", doc.Faults)
	}
	if doc.Faults.MaxLevel() != media.LevelFatal {
		t.Fatalf("level = %v, want fatal (other tracks keep parsing)", doc.Faults.MaxLevel())
	}
	if !doc.StoppedTracks[7] {
		t.Fatal("track 7 should be marked stopped")
	}
}

func TestTrackGraphCycleAndUnknownDep(t *testing.T) {
	doc := ebml.NewDocument()
	doc.Tracks = []ebml.Track{
		{Number: 1, Kind: media.KindVideo, DependsOn: []uint64{2}},
		{Number: 2, Kind: media.KindAudio, DependsOn: []uint64{1}},
	}
	if _, err := ebml.NewGraph(doc); err == nil {
		t.Fatal("expected a cycle error")
	}
	if !doc.Faults.Has(media.CodeDependencyCycle) {
		t.Fatalf("faults = %v, want dependency_cycle", doc.Faults)
	}

	doc = ebml.NewDocument()
	doc.Tracks = []ebml.Track{
		{Number: 1, Kind: media.KindVideo, DependsOn: []uint64{42}},
	}
	if _, err := ebml.NewGraph(doc); err == nil {
		t.Fatal("expected an unknown-track error")
	}
	if !doc.Faults.Has(media.CodeUnknownTrack) {
		t.Fatalf("faults = %v, want unknown_track", doc.Faults)
	}
}

func TestFinishOrderHonoursDependencies(t *testing.T) {
	doc := ebml.NewDocument()
	doc.Tracks = []ebml.Track{
		{Number: 3, Kind: media.KindCue, DependsOn: []uint64{1}},
		{Number: 1, Kind: media.KindVideo, DependsOn: []uint64{2}},
		{Number: 2, Kind: media.KindAudio},
	}
	g, err := ebml.NewGraph(doc)
	if err != nil {
		t.Fatal(err)
	}
	order, err := g.FinishOrder()
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 3 || order[2] != 3 {
		t.Fatalf("finish order = %v, want 3 last", order)
	}
	anchor, err := g.Anchor()
	if err != nil || anchor != 2 {
		t.Fatalf("anchor = %d, err = %v, want 2", anchor, err)
	}
}

func TestMissingAudioAnchorIsError(t *testing.T) {
	doc := ebml.NewDocument()
	doc.Tracks = []ebml.Track{{Number: 1, Kind: media.KindVideo}}
	g, err := ebml.NewGraph(doc)
	if err == nil {
		if _, err = g.Anchor(); err == nil {
			t.Fatal("expected a missing-anchor error")
		}
	}
}
