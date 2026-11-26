package fixture

import (
	"testing"

	"bq/internal/manifest"
)

func TestBuildDeterministic(t *testing.T) {
	opts := DefaultOptions(41)
	a := Build(opts)
	b := Build(opts)
	if len(a.Packets) != len(b.Packets) {
		t.Fatal("packet count differs across same-seed builds")
	}
	for i := range a.Packets {
		if a.Packets[i] != b.Packets[i] {
			t.Fatalf("packet %d differs across same-seed builds", i)
		}
	}
	if len(a.Anchors) != len(b.Anchors) {
		t.Fatal("anchors not deterministic")
	}
	for i := range a.Anchors {
		if a.Anchors[i] != b.Anchors[i] {
			t.Fatalf("anchor %d not deterministic", i)
		}
	}
}

func TestBuildHasThreeVideoAndAudio(t *testing.T) {
	m := Build(DefaultOptions(1))
	if m.VideoStreamCount() != 3 {
		t.Fatalf("want 3 video tracks, got %d", m.VideoStreamCount())
	}
	kinds := map[string]int{}
	for _, s := range m.Streams {
		kinds[s.Kind]++
		if s.Container != "mp4" {
			t.Fatal("all streams must be mp4")
		}
		if s.Kind == "video" && s.Codec != "h264" {
			t.Fatal("video must be h264")
		}
		if s.Kind == "audio" && s.Codec != "aac" {
			t.Fatal("audio must be aac")
		}
		if s.StartPTS <= 0 {
			t.Fatalf("stream %d must have non-zero start PTS", s.ID)
		}
	}
	if kinds["audio"] != 1 {
		t.Fatalf("want 1 audio track, got %d", kinds["audio"])
	}
}

func TestBuildSpansThirtyMinutes(t *testing.T) {
	opts := DefaultOptions(1)
	m := Build(opts)
	if m.DurationS != 1800 {
		t.Fatalf("want 1800s, got %d", m.DurationS)
	}
	for _, s := range m.Streams {
		pkts := mediaOf(m, s.ID)
		last := pkts[len(pkts)-1]
		// Final packet should sit near the end of the run.
		if last.RawPTS < s.TimebaseNum*(m.DurationS-2) {
			t.Fatalf("stream %d does not span full duration, last raw pts %d", s.ID, last.RawPTS)
		}
	}
}

func TestWrapPeriodConfigured(t *testing.T) {
	opts := DefaultOptions(1)
	m := Build(opts)
	if len(m.Injection.WrappedStreams) != len(m.Streams) {
		t.Fatal("wrap injection should mark every stream")
	}
	for _, s := range m.Streams {
		if s.WrapPeriod <= 0 {
			t.Fatalf("stream %d missing wrap period", s.ID)
		}
		// A 30-minute run with these periods must wrap at least twice.
		if s.WrapPeriod > s.TimebaseNum*900 {
			t.Fatalf("stream %d wrap period too long to wrap in 30min", s.ID)
		}
	}
}

func TestInjectionDescriptors(t *testing.T) {
	opts := DefaultOptions(7)
	opts.InjectDup = true
	opts.InjectTrunc = true
	opts.AnchorConflicts = 2
	m := Build(opts)
	if len(m.Injection.Duplicates) != 1 {
		t.Fatalf("want 1 duplicate, got %d", len(m.Injection.Duplicates))
	}
	if len(m.Injection.TruncateAfterSeq) != 1 {
		t.Fatalf("want 1 truncation, got %d", len(m.Injection.TruncateAfterSeq))
	}
	conflicts := 0
	for _, a := range m.Anchors {
		if a.Conflicts {
			conflicts++
		}
	}
	if conflicts != 2 {
		t.Fatalf("want 2 conflicting anchors, got %d", conflicts)
	}
	if m.Injection.LossRate != 0.02 {
		t.Fatalf("loss rate not carried: %v", m.Injection.LossRate)
	}
}

func TestPacketsCarryProvenanceAndParity(t *testing.T) {
	m := Build(DefaultOptions(3))
	var media, parity int
	for _, p := range m.Packets {
		if p.SourceOffset < 0 {
			t.Fatal("source offset must be non-negative")
		}
		if p.Kind == manifest.KindMedia {
			media++
			if p.DurationTick <= 0 {
				t.Fatal("VFR duration must be positive")
			}
		} else {
			parity++
		}
	}
	if media == 0 || parity == 0 {
		t.Fatal("expected both media and parity packets")
	}
	// Verify the XOR identity holds group by group for every stream.
	groups := manifest.GroupPackets(m.Packets)
	for gid, pkts := range groups {
		var mediaXOR byte
		var par byte
		parityCount := 0
		for _, p := range pkts {
			if p.Kind == manifest.KindMedia {
				mediaXOR ^= p.XOR
			} else {
				par = p.XOR
				parityCount++
			}
		}
		if parityCount != 1 {
			t.Fatalf("group %d has %d parity packets", gid, parityCount)
		}
		if mediaXOR != par {
			t.Fatalf("group %d XOR identity broken: %d != %d", gid, mediaXOR, par)
		}
	}
}

func TestSparseKeyframes(t *testing.T) {
	m := Build(DefaultOptions(5))
	for _, s := range m.Streams {
		if s.Kind != manifest.KindVideo {
			continue
		}
		var keyframes int
		for _, p := range mediaOf(m, s.ID) {
			if p.Keyframe {
				keyframes++
			}
		}
		// keyframeEvery=30 over ~1800 packets -> sparse, around 60 frames.
		if keyframes == 0 || keyframes > 120 {
			t.Fatalf("stream %d unexpected keyframe count %d", s.ID, keyframes)
		}
	}
}

func TestDriftModelMovesSourcePTS(t *testing.T) {
	bp := blueprints()[0]
	base := sourcePTSAt(bp, 0)
	later := sourcePTSAt(bp, 600)
	// Non-zero drift offset/segments must make later differ from plain linear.
	plain := bp.startPTS + 600*bp.timebaseNum
	if later == plain {
		t.Fatal("drift model did not perturb source PTS")
	}
	if base < bp.startPTS {
		t.Fatal("start anchor before declared start PTS")
	}
}
