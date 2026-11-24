package pipeline

import (
	"context"
	"path/filepath"
	"testing"

	"mediadeliver/internal/fixture"
	"mediadeliver/internal/manifest"
	"mediadeliver/internal/model"
)

// TestCorruptionPropagatesFromDemuxToCheckpoint proves the full chain: a
// damaged MP4 length field is detected at demux, classified KindCorruptMedia
// with its first position, the material is isolated, and no resumable
// half-finished checkpoint is left for the failed segment.
func TestCorruptionPropagatesFromDemuxToCheckpoint(t *testing.T) {
	man, err := manifest.Parse(stringReader(testManifestText))
	if err != nil {
		t.Fatal(err)
	}
	src := buildSources(t)

	// Corrupt cam2's mdat length field (top-level boxes: ftyp=0, moov=1, mdat=2).
	corrupt, err := fixture.CorruptMP4Length(src["cam2"].Data, 2, -64)
	if err != nil {
		t.Fatal(err)
	}
	src["cam2"].Data = corrupt

	cpPath := filepath.Join(t.TempDir(), "cp.json")
	cfg := baseConfig()
	cfg.CorruptEvery = 0 // isolate the corruption path from write-fault retry
	rn := NewRunner(cpPath, cfg)
	rep, err := rn.Run(context.Background(), man, []byte(testManifestText), src)
	if err != nil {
		t.Fatalf("isolated demux corruption must not abort the run: %v", err)
	}

	// The damaged material must be recorded with KindCorruptMedia + position.
	var found bool
	for _, e := range rep.TrackErrors {
		if model.KindOf(e) == model.KindCorruptMedia && model.PositionOf(e) >= 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("demux corruption must propagate a positioned KindCorruptMedia: %+v", rep.TrackErrors)
	}

	// segB depends solely on cam2; it must not appear as a completed segment.
	snap := rn.store.Snapshot()
	for name := range snap.Completed {
		if name == "segB" {
			t.Fatal("corrupt segB must not leave a resumable checkpoint entry")
		}
	}
	// Healthy segments segA and segC still land.
	if len(rep.Segments) != 2 {
		t.Fatalf("want exactly the 2 healthy segments, got %d", len(rep.Segments))
	}
}

// TestTruncatedRTPPropagatesThroughDemux exercises the RTP half of the chain.
func TestTruncatedRTPPropagatesThroughDemux(t *testing.T) {
	man, err := manifest.Parse(stringReader(testManifestText))
	if err != nil {
		t.Fatal(err)
	}
	src := buildSources(t)
	torn, err := fixture.TruncateRTPCapture(src["rec1"].Data, 5)
	if err != nil {
		t.Fatal(err)
	}
	src["rec1"].Data = torn

	cpPath := filepath.Join(t.TempDir(), "cp.json")
	cfg := baseConfig()
	cfg.CorruptEvery = 0
	rn := NewRunner(cpPath, cfg)
	rep, err := rn.Run(context.Background(), man, []byte(testManifestText), src)
	if err != nil {
		t.Fatalf("isolated RTP corruption must not abort the run: %v", err)
	}
	hasPositioned := false
	for _, e := range rep.TrackErrors {
		if model.KindOf(e) == model.KindCorruptMedia && model.PositionOf(e) == 5 {
			hasPositioned = true
		}
	}
	if !hasPositioned {
		t.Fatalf("truncated RTP packet 5 must propagate with position 5: %+v", rep.TrackErrors)
	}
	if _, ok := rn.store.Snapshot().Completed["segC"]; ok {
		t.Fatal("segC backed by torn RTP must not be committed")
	}
}
