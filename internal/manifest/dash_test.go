package manifest

import "testing"

const mpd = `<?xml version="1.0"?>
<MPD><Period><AdaptationSet><Representation>
<SegmentTemplate timescale="48000" startNumber="1" media="seg$Number$.m4s">
<SegmentTimeline>
<S t="0" d="192000" r="1"/>
<S d="96000"/>
</SegmentTimeline>
</SegmentTemplate>
</Representation></AdaptationSet></Period></MPD>`

func TestParseDASHGood(t *testing.T) {
	p := ParseDASH("live.mpd", []byte(mpd))
	if p.Truncated || len(p.Faults) != 0 {
		t.Fatalf("faults: %+v", p.Faults)
	}
	// r=1 展开为 2 条，再加 1 条
	if len(p.Segments) != 3 {
		t.Fatalf("segments: %+v", p.Segments)
	}
	if p.Segments[0].URI != "seg1.m4s" || p.Segments[2].URI != "seg3.m4s" {
		t.Fatalf("uri: %+v", p.Segments)
	}
	if p.Segments[0].Duration.Millis() != 4000 || p.Segments[2].Duration.Millis() != 2000 {
		t.Fatalf("duration: %+v", p.Segments)
	}
	if p.Duration.Millis() != 10000 {
		t.Fatalf("total: %v", p.Duration.Millis())
	}
}

func TestParseDASHTruncated(t *testing.T) {
	trunc := `<MPD><Period><Representation>
<SegmentTemplate timescale="48000" media="s$Number$.m4s">
<SegmentTimeline>
<S t="0" d="48000"/>
<S d="48000"`
	p := ParseDASH("t.mpd", []byte(trunc))
	if !p.Truncated {
		t.Fatal("expected truncated")
	}
	if len(p.Segments) != 1 {
		t.Fatalf("prefix kept: %+v", p.Segments)
	}
	if p.Faults[0].Offset < 0 || !p.Faults[0].Recoverable {
		t.Fatalf("fault: %+v", p.Faults[0])
	}
}

func TestParseDASHBadS(t *testing.T) {
	bad := `<MPD><SegmentTemplate timescale="1000" media="s$Number$.m4s"><SegmentTimeline><S d="0"/><S d="500"/></SegmentTimeline></SegmentTemplate></MPD>`
	p := ParseDASH("b.mpd", []byte(bad))
	if len(p.Segments) != 1 || len(p.Faults) == 0 {
		t.Fatalf("bad S: %+v", p)
	}
}
