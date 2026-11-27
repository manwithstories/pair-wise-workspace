package fixture

import (
	"bytes"
	"testing"

	"fastpost/internal/metadata"
)

func TestGenerateDeterministic(t *testing.T) {
	a := Generate(2601, 12, FaultSpec{})
	b := Generate(2601, 12, FaultSpec{})
	if len(a.Tracks) != 2 {
		t.Fatalf("tracks = %d, want 2", len(a.Tracks))
	}
	for i := range a.Tracks {
		if !bytes.Equal(a.Tracks[i].SEI, b.Tracks[i].SEI) {
			t.Fatal("same seed must produce identical SEI blobs")
		}
		if !bytes.Equal(a.Tracks[i].Subtitles, b.Tracks[i].Subtitles) {
			t.Fatal("same seed must produce identical subtitle blobs")
		}
	}
	// A different seed must change at least one blob.
	c := Generate(2602, 12, FaultSpec{})
	if bytes.Equal(a.Tracks[0].SEI, c.Tracks[0].SEI) {
		t.Fatal("different seeds must produce different SEI blobs")
	}
}

func TestGeneratedBlobsParse(t *testing.T) {
	set := Generate(2601, 12, FaultSpec{})
	for _, tr := range set.Tracks {
		sei, err := metadata.ParseSEI(tr.ID, tr.SEI, metadata.RationalLike{Num: tr.RateNum, Den: tr.RateDen})
		if err != nil {
			t.Fatalf("SEI parse on %s: %v", tr.ID, err)
		}
		if len(sei) != 12 {
			t.Fatalf("%s SEI events = %d, want 12", tr.ID, len(sei))
		}
		subs, err := metadata.ParseSubtitle(tr.ID, tr.Subtitles, rate)
		if err != nil || len(subs) != 12 {
			t.Fatalf("subtitles %s: %d events err=%v", tr.ID, len(subs), err)
		}
		chapters, err := metadata.ParseChapter(tr.ID, tr.Chapters, rate)
		if err != nil || len(chapters) != 12 {
			t.Fatalf("chapters %s: %d events err=%v", tr.ID, len(chapters), err)
		}
		tags, err := metadata.ParseID3(tr.ID, tr.ID3, rate)
		if err != nil || len(tags) != 12 {
			t.Fatalf("id3 %s: %d events err=%v", tr.ID, len(tags), err)
		}
		msgs, err := metadata.ParseEMSG(tr.ID, tr.EMSG, rate)
		if err != nil || len(msgs) != 12 {
			t.Fatalf("emsg %s: %d events err=%v", tr.ID, len(msgs), err)
		}
	}
}

func TestBoundaryEventPresent(t *testing.T) {
	// The first SEI event sits at position 0 and the second at one full
	// segment duration, i.e. exactly on the first shared boundary.
	set := Generate(2601, 12, FaultSpec{})
	sei, err := metadata.ParseSEI(set.Tracks[0].ID, set.Tracks[0].SEI,
		metadata.RationalLike{Num: set.Tracks[0].RateNum, Den: set.Tracks[0].RateDen})
	if err != nil {
		t.Fatal(err)
	}
	if sei[1].Event.Position != set.Tracks[0].SegmentTicks[1] {
		t.Fatalf("second event %d not on boundary %d",
			sei[1].Event.Position, set.Tracks[0].SegmentTicks[1])
	}
}

func TestSEITruncateFault(t *testing.T) {
	fault := FaultSpec{Kind: "sei-truncate", Offset: 4096}
	set := Generate(2601, 12, fault)
	if set.FaultTrack != "cam-a" {
		t.Fatalf("fault track = %q, want cam-a", set.FaultTrack)
	}
	if int64(len(set.Tracks[0].SEI)) != fault.Offset {
		t.Fatalf("SEI blob len = %d, want cut at %d", len(set.Tracks[0].SEI), fault.Offset)
	}
	// The truncated blob must surface a structured truncation fault.
	_, err := metadata.ParseSEI(set.Tracks[0].ID, set.Tracks[0].SEI,
		metadata.RationalLike{Num: set.Tracks[0].RateNum, Den: set.Tracks[0].RateDen})
	if err == nil {
		t.Fatal("truncated SEI blob must fail parsing")
	}
}

func TestFractionalClockAndDistinctOrigins(t *testing.T) {
	set := Generate(2601, 12, FaultSpec{})
	// Fractional 30000/1001 clock is carried by every track.
	for _, tr := range set.Tracks {
		if tr.RateNum != 30000 || tr.RateDen != 1001 {
			t.Fatalf("track %s rate = %d/%d, want 30000/1001", tr.ID, tr.RateNum, tr.RateDen)
		}
	}
	// The two cameras exercise distinct audio layouts as separate groups.
	if set.Tracks[0].Channels == set.Tracks[1].Channels {
		t.Fatal("cameras must expose distinct channel layouts")
	}
}

func TestParseFaultSpec(t *testing.T) {
	fs, err := ParseFaultSpec("sei-truncate:4096")
	if err != nil || fs.Kind != "sei-truncate" || fs.Offset != 4096 {
		t.Fatalf("parse = %+v err=%v", fs, err)
	}
	if _, err := ParseFaultSpec("bad:1"); err == nil {
		t.Fatal("unknown fault kind must error")
	}
	if _, err := ParseFaultSpec("sei-truncate:abc"); err == nil {
		t.Fatal("non-numeric offset must error")
	}
	if _, err := ParseFaultSpec(""); err != nil {
		t.Fatalf("empty spec should be allowed, got %v", err)
	}
}
