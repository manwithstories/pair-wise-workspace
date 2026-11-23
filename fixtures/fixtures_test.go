package fixtures

import (
	"bytes"
	"testing"

	"mediad/diag"
	"mediad/subtitle"
)

func TestGenerateFeedsDeterministic(t *testing.T) {
	spec := Spec{Seed: 42, DurationMS: 90000, Bitrates: []int{600, 1200, 2400},
		KeyframeEvery: 90, PhaseJitter: 1}
	a, err := GenerateFeeds(spec)
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateFeeds(spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 3 {
		t.Fatalf("feeds = %d, want 3", len(a))
	}
	for i := range a {
		if a[i].RepID != b[i].RepID || len(a[i].Frames) != len(b[i].Frames) {
			t.Fatalf("feed %d not deterministic", i)
		}
		for f := range a[i].Frames {
			if a[i].Frames[f].Keyframe != b[i].Frames[f].Keyframe ||
				a[i].Frames[f].Time.Cmp(b[i].Frames[f].Time) != 0 {
				t.Fatalf("frame %d of feed %d differs across runs", f, i)
			}
		}
	}
	// 90s at 30fps = 2700 frames per feed.
	if len(a[0].Frames) != 2700 {
		t.Fatalf("frame count = %d, want 2700", len(a[0].Frames))
	}
}

func TestAnchorGridHasNoJitter(t *testing.T) {
	feeds, _ := GenerateFeeds(Spec{Seed: 1, DurationMS: 9000, Bitrates: []int{600, 1200},
		KeyframeEvery: 90, PhaseJitter: 1})
	for i, f := range feeds[0].Frames {
		want := i%90 == 0
		if f.Keyframe != want {
			t.Fatalf("anchor keyframe grid drifted at %d", i)
		}
	}
}

func TestCorruptAndTruncateLocateBytes(t *testing.T) {
	data := SRTInput(400) // long enough to span both fixture offsets
	if len(data) <= 8192 {
		t.Fatalf("test fixture too short: %d bytes", len(data))
	}
	corrupt := CorruptAt(data, 4096)
	// Located corruption changes exactly the byte at offset 4096.
	diff := 0
	for i := range data {
		if data[i] != corrupt[i] {
			diff++
			if i != 4096 {
				t.Fatalf("corruption at unexpected byte %d", i)
			}
		}
	}
	if diff != 1 {
		t.Fatalf("corruption changed %d bytes, want 1", diff)
	}
	trunc := TruncateAt(data, 8192)
	if len(trunc) != 8192 {
		t.Fatalf("truncated length = %d, want 8192", len(trunc))
	}
	if !bytes.Equal(trunc, data[:8192]) {
		t.Fatal("truncated fixture must be a strict prefix")
	}
}

func TestSRTInputParsable(t *testing.T) {
	// The generated SRT must round-trip through the real streaming parser.
	data := SRTInput(4)
	chunks := [][]int{{1}, {7}, {len(data)}}
	for _, sizes := range chunks {
		p := subtitle.NewSRT("fixture", diag.DefaultLimits())
		cues, err := parseWithChunks(p, data, sizes)
		if err != nil {
			t.Fatalf("fixture SRT not parsable: %v", err)
		}
		if len(cues) != 4 {
			t.Fatalf("cues = %d, want 4", len(cues))
		}
	}
}

// parseWithChunks feeds data through the parser in the given repeating chunk
// sizes, mirroring how the pipeline delivers arbitrary buffer boundaries.
func parseWithChunks(p *subtitle.SRTParser, data []byte, sizes []int) ([]subtitle.Cue, error) {
	pos := 0
	for pos < len(data) {
		n := sizes[pos%len(sizes)]
		if pos+n > len(data) {
			n = len(data) - pos
		}
		if err := p.Write(data[pos : pos+n]); err != nil {
			return nil, err
		}
		pos += n
	}
	return p.Close()
}

func TestCyclicGraphRejected(t *testing.T) {
	g, err := CyclicGraph()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = g.Topo()
	if diag.CodeOf(err) != diag.EGraphCycle {
		t.Fatalf("cyclic fixture must be E_GRAPH_CYCLE, got %v", err)
	}
}

func TestValidGraphAccepts(t *testing.T) {
	g, err := ValidGraph([]string{"v-600", "v-1200"})
	if err != nil {
		t.Fatal(err)
	}
	// Two representations plus timecode and subtitle tracks.
	order, cyc, err := g.Topo()
	if err != nil || cyc != nil || len(order) != 4 {
		t.Fatalf("valid graph rejected: order=%v err=%v", order, err)
	}
}

func TestSpecValidation(t *testing.T) {
	if _, err := GenerateFeeds(Spec{DurationMS: 0, Bitrates: []int{600}});
		diag.CodeOf(err) != diag.EParam {
		t.Fatalf("bad spec must be E_PARAM, got %v", err)
	}
}

func TestLocateByteCorruption(t *testing.T) {
	data := SRTInput(400)
	for _, at := range []int{0, 4096, len(data) - 1} {
		corrupt := CorruptAt(data, at)
		got := LocateByteCorruption(data, corrupt)
		want := at
		if at >= len(data) {
			want = len(data) - 1
		}
		if got != want {
			t.Fatalf("LocateByteCorruption at %d = %d, want %d", at, got, want)
		}
	}
	if LocateByteCorruption(data, data) != -1 {
		t.Fatal("identical buffers must report no corruption")
	}
}
