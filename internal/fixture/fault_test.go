package fixture

import (
	"os"
	"testing"

	"mediascan/internal/analyze"
	"mediascan/internal/index"
	"mediascan/internal/manifest"
	"mediascan/internal/model"
	"mediascan/internal/stream"
)

func scanWorkspace(t *testing.T, work string) (*index.Index, *manifest.Resolver) {
	t.Helper()
	res, err := manifest.NewResolver(work)
	if err != nil { t.Fatal(err) }
	tl, err := manifest.Load(res, "hls/media.m3u8")
	if err != nil { t.Fatal(err) }
	manifest.MarkPresence(res, tl)
	idx, err := index.Scan(res, tl, index.Options{})
	if err != nil { t.Fatal(err) }
	// The tracking sort hides a wrap, so this test checks the raw per-segment
	// base times exactly as the analyzer sees them.
	return idx, res
}

// locate walks the four self-generated fault samples and returns the findings
// each must produce. Every range is checked against the real file size.
func TestFaultSamplesLocateExpectedRanges(t *testing.T) {
	t.Run("truncated box", func(t *testing.T) {
		work := t.TempDir()
		l, err := Generate(work)
		if err != nil { t.Fatal(err) }
		scan, err := stream.ScanSegment(nil, "seg-04.m4s", SegmentPath(work, l.TruncatedIdx), 0, -1)
		if err != nil { t.Fatal(err) }
		f, ok := scan.FirstUndecodable()
		if !ok || f.Kind != model.FaultTruncatedBox {
			t.Fatalf("fault=%+v ok=%v", f, ok)
		}
		info, _ := os.Stat(SegmentPath(work, l.TruncatedIdx))
		if f.ByteStart < 0 || f.ByteEnd >= info.Size() || f.ByteEnd < f.ByteStart {
			t.Fatalf("range %d-%d outside file %d", f.ByteStart, f.ByteEnd, info.Size())
		}
	})

	t.Run("out of range length", func(t *testing.T) {
		work := t.TempDir()
		l, err := GenerateOutOfRange(work)
		if err != nil { t.Fatal(err) }
		scan, err := stream.ScanSegment(nil, "seg-05.m4s", SegmentPath(work, l.OutOfRangeIdx), 0, -1)
		if err != nil { t.Fatal(err) }
		f, ok := scan.FirstUndecodable()
		if !ok || f.Kind != model.FaultBoxBounds {
			t.Fatalf("fault=%+v ok=%v", f, ok)
		}
		if f.ByteEnd < f.ByteStart {
			t.Fatalf("bad range %d-%d", f.ByteStart, f.ByteEnd)
		}
	})

	t.Run("missing segment", func(t *testing.T) {
		work := t.TempDir()
		l, err := Generate(work)
		if err != nil { t.Fatal(err) }
		idx, _ := scanWorkspace(t, work)
		if idx.FirstMiss != SegmentURI(l.MissingIdx) {
			t.Fatalf("first missing=%q want %q", idx.FirstMiss, SegmentURI(l.MissingIdx))
		}
		// The partial timeline still holds every other slot.
		if len(idx.Entries) != SegmentCount || idx.Scanned != SegmentCount-1 {
			t.Fatalf("partial timeline: entries=%d scanned=%d", len(idx.Entries), idx.Scanned)
		}
	})

	t.Run("timestamp wrap", func(t *testing.T) {
		work := t.TempDir()
		l, err := GenerateWrap(work)
		if err != nil { t.Fatal(err) }
		idx, res := scanWorkspace(t, work)
		init, err := stream.LoadInit(l.VideoInit)
		if err != nil { t.Fatal(err) }
		tracks, err := analyze.BuildTracks(idx, res, init)
		if err != nil { t.Fatal(err) }
		var video, audio analyze.Track
		for _, tr := range tracks {
			if tr.Kind == "video" { video = tr }
			if tr.Kind == "audio" { audio = tr }
		}
		sync, err := analyze.Align(video, audio, 40)
		if err != nil { t.Fatal(err) }
		if sync.WrapSpans == 0 {
			t.Fatal("timestamp wrap not detected")
		}
		found := false
		for _, f := range sync.Faults {
			if f.Kind == model.FaultTimestamp { found = true }
		}
		if !found { t.Fatalf("wrap not propagated to faults: %+v", sync.Faults) }
	})
}
