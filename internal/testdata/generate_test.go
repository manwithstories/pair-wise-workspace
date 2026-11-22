package testdata

import (
	"bytes"
	"testing"

	"github.com/pion/rtp"
)

func TestBuildDeterministic(t *testing.T) {
	p := DefaultParams(17)
	p.VFR = true
	f1, err := Build(p)
	if err != nil {
		t.Fatal(err)
	}
	f2, err := Build(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f1.MP4, f2.MP4) {
		t.Fatal("same seed must yield byte-identical MP4")
	}
	if len(f1.RTP) != f1.Params.VideoFrames {
		t.Fatalf("rtp packets = %d", len(f1.RTP))
	}
	if len(f1.ExpectedKeyframes) == 0 {
		t.Fatal("fixture must contain keyframes")
	}
}

func TestRTPWrapOccurs(t *testing.T) {
	p := DefaultParams(17)
	p.WrapAt = 4294967200
	f, err := Build(p)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := false
	for i := 1; i < len(f.RTP); i++ {
		if f.RTP[i].Timestamp < f.RTP[i-1].Timestamp {
			wrapped = true
		}
	}
	if !wrapped {
		t.Fatal("wrap-at fixture must produce a 32-bit rollover")
	}
	// Every packet must round-trip through the pion RTP header codec.
	raw, err := MarshalRTP(f.RTP)
	if err != nil {
		t.Fatal(err)
	}
	var decoded rtp.Packet
	if err := decoded.Unmarshal(raw[0]); err != nil {
		t.Fatal(err)
	}
	if decoded.Timestamp != f.RTP[0].Timestamp || decoded.SSRC != f.RTP[0].SSRC {
		t.Fatal("RTP header round-trip mismatch")
	}
}

func TestSilenceGroundTruth(t *testing.T) {
	p := DefaultParams(17)
	f, err := Build(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.ExpectedSilences) == 0 {
		t.Fatal("expected at least one synthetic silence span")
	}
	for _, s := range f.ExpectedSilences {
		if s.End <= s.Start {
			t.Fatalf("degenerate silence %+v", s)
		}
	}
}

func TestFiveEventFamilies(t *testing.T) {
	f, err := Build(DefaultParams(17))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[Kind]int{}
	for _, e := range f.Events {
		seen[e.Kind]++
	}
	for _, k := range []Kind{KindSubtitle, KindChapter, KindID3, KindEmsg, KindSEI} {
		if seen[k] == 0 {
			t.Fatalf("missing event family %s", k)
		}
	}
}

func TestCorruptionSwitches(t *testing.T) {
	p := DefaultParams(17)
	p.CorruptBox = true
	p.Truncate = true
	f, err := Build(p)
	if err != nil {
		t.Fatal(err)
	}
	good, _ := Build(DefaultParams(17))
	if bytes.Equal(f.MP4, good.MP4) {
		t.Fatal("corrupted fixture must differ from clean fixture")
	}
}
