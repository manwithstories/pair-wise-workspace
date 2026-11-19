package timeline

import (
	"testing"

	"mediamix/internal/core"
)

func TestTimeBaseRoundTrip(t *testing.T) {
	tb, err := NewTimeBase(90000)
	if err != nil {
		t.Fatal(err)
	}
	us, err := tb.ToMicros(90000) // one second
	if err != nil {
		t.Fatal(err)
	}
	if us != MicrosPerSecond {
		t.Fatalf("90000 ticks @90k = %d us, want %d", us, MicrosPerSecond)
	}
	ticks, err := tb.FromMicros(us)
	if err != nil || ticks != 90000 {
		t.Fatalf("round trip = %d, %v", ticks, err)
	}
}

func TestTimeBaseRejectsZeroAndNegative(t *testing.T) {
	if _, err := NewTimeBase(0); err == nil {
		t.Fatal("zero timescale must be rejected, never guessed")
	}
	tb, _ := NewTimeBase(48000)
	if _, err := tb.ToMicros(-1); core.CodeOf(err) != core.CodeBadTimestamp {
		t.Fatalf("negative timestamp code = %s", core.CodeOf(err))
	}
}

func TestSpanOverlap(t *testing.T) {
	a := Span{Start: 0, End: 1000}
	b := Span{Start: 500, End: 1500}
	c := Span{Start: 1000, End: 2000}
	if !a.Overlaps(b) {
		t.Fatal("a and b overlap")
	}
	if a.Overlaps(c) {
		t.Fatal("touching intervals must not overlap (half-open)")
	}
}

func TestNewSpanRejectsNegativeDuration(t *testing.T) {
	if _, err := NewSpan(0, -1); err == nil {
		t.Fatal("negative duration must be rejected")
	}
}

func TestGraphOrderRespectsDependencies(t *testing.T) {
	g, _ := NewGraph(16)
	for _, n := range []Node{
		{Track: "audio", Kind: KindAudio, DependsOn: []string{"proxy"}, Span: Span{0, 100}},
		{Track: "proxy", Kind: KindVideo, DependsOn: []string{"cam"}, Span: Span{0, 100}},
		{Track: "cam", Kind: KindVideo, Span: Span{0, 100}},
	} {
		if err := g.Add(n); err != nil {
			t.Fatal(err)
		}
	}
	order, err := g.Order()
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, id := range order {
		pos[id] = i
	}
	if !(pos["cam"] < pos["proxy"] && pos["proxy"] < pos["audio"]) {
		t.Fatalf("bad order: %v", order)
	}
}

func TestGraphDetectsCycle(t *testing.T) {
	g, _ := NewGraph(16)
	g.Add(Node{Track: "a", Kind: KindVideo, DependsOn: []string{"b"}, Span: Span{0, 1}})
	g.Add(Node{Track: "b", Kind: KindVideo, DependsOn: []string{"a"}, Span: Span{0, 1}})
	errs := g.Validate()
	found := false
	for _, e := range errs {
		if e.Code == core.CodeCycle {
			found = true
		}
	}
	if !found {
		t.Fatalf("cycle not detected: %+v", errs)
	}
}

func TestGraphDetectsMissingDependency(t *testing.T) {
	g, _ := NewGraph(16)
	g.Add(Node{Track: "a", Kind: KindVideo, DependsOn: []string{"ghost"}, Span: Span{0, 1}})
	errs := g.Validate()
	found := false
	for _, e := range errs {
		if e.Code == core.CodeMissingDep {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing dependency not detected: %+v", errs)
	}
}

func TestGraphDetectsExclusiveOverlap(t *testing.T) {
	g, _ := NewGraph(16)
	g.Add(Node{Track: "take1", Kind: KindVideo, ExclusiveGroup: "slot", Span: Span{0, 1000}})
	g.Add(Node{Track: "take2", Kind: KindVideo, ExclusiveGroup: "slot", Span: Span{500, 1500}})
	errs := g.Validate()
	found := false
	for _, e := range errs {
		if e.Code == core.CodeOverlap {
			found = true
		}
	}
	if !found {
		t.Fatalf("overlap in exclusive group not detected: %+v", errs)
	}
}

func TestGraphAllowsConcurrentAngles(t *testing.T) {
	// Two camera angles cover the same interval; this is not a fault.
	g, _ := NewGraph(16)
	g.Add(Node{Track: "cam-a", Kind: KindVideo, Span: Span{0, 6000}})
	g.Add(Node{Track: "cam-b", Kind: KindVideo, Span: Span{0, 6000}})
	if errs := g.Validate(); len(errs) != 0 {
		t.Fatalf("concurrent angles must be valid: %+v", errs)
	}
}

func TestGraphRejectsDuplicateAndSelfDependency(t *testing.T) {
	g, _ := NewGraph(16)
	if err := g.Add(Node{Track: "a", Kind: KindVideo, Span: Span{0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(Node{Track: "a", Kind: KindVideo, Span: Span{0, 1}}); core.CodeOf(err) != core.CodeBadGraph {
		t.Fatalf("duplicate code = %s", core.CodeOf(err))
	}
	if err := g.Add(Node{Track: "b", Kind: KindVideo, DependsOn: []string{"b"}, Span: Span{0, 1}}); core.CodeOf(err) != core.CodeCycle {
		t.Fatalf("self dependency code = %s", core.CodeOf(err))
	}
}

func TestGraphRejectsOverLimit(t *testing.T) {
	g, _ := NewGraph(2)
	g.Add(Node{Track: "a", Kind: KindVideo, Span: Span{0, 1}})
	g.Add(Node{Track: "b", Kind: KindVideo, Span: Span{0, 1}})
	if err := g.Add(Node{Track: "c", Kind: KindVideo, Span: Span{0, 1}}); core.CodeOf(err) != core.CodeOverLimit {
		t.Fatalf("over limit code = %s", core.CodeOf(err))
	}
}

func TestMergeSpans(t *testing.T) {
	in := []Span{{0, 10}, {10, 20}, {30, 40}}
	out := MergeSpans(in)
	if len(out) != 2 || out[0].End != 20 || out[1].Start != 30 {
		t.Fatalf("merge = %+v", out)
	}
}

func TestSyncDelta(t *testing.T) {
	b1, _ := NewTimeBase(90000)
	c1, err := NewClock("cam-a", b1, 1000)
	if err != nil {
		t.Fatal(err)
	}
	c2, _ := NewClock("cam-b", b1, 3000)
	d, err := SyncDelta(c1, c2)
	if err != nil || d != 2000 {
		t.Fatalf("sync delta = %d (%v), want 2000", d, err)
	}
}
