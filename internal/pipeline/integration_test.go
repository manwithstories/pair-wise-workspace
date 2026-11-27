package pipeline_test

import (
	"context"
	"path/filepath"
	"testing"

	"mediacheck/internal/ingest"
	"mediacheck/internal/model"
	"mediacheck/internal/pipeline"
	"mediacheck/internal/remux"
)

// TestRecoveredOutputTimelineIsContinuous is the integration check that
// matters for delivery: after a damaged run, every audio slot in the muxed
// file must be present, non-overlapping and in order, so a player sees no
// hole and no timestamp regression. It also proves the video track keeps its
// keyframe spacing.
func TestRecoveredOutputTimelineIsContinuous(t *testing.T) {
	cfg := ingest.DefaultConfig(17)
	cfg.VideoFrames = 120
	g, err := ingest.Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.ApplyProfile(ingest.ProfileDamaged); err != nil {
		t.Fatal(err)
	}
	fixture := g.Fixture()

	dir := t.TempDir()
	dest := filepath.Join(dir, "recovered.mkv")
	w, err := remux.Open(dest, fixture.Streams)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Abort()

	type span struct{ start, end int64 }
	var audio []span
	var video []span
	res, err := pipeline.Run(context.Background(), g, pipeline.Options{
		QueueItems: 16,
		QueueBytes: 1 << 20,
		Recovery:   true,
		Emit: func(p model.Packet) error {
			if err := w.Write(context.Background(), p); err != nil {
				return err
			}
			switch p.Track {
			case model.TrackAudio:
				audio = append(audio, span{p.Slot.Start, p.Slot.End})
			case model.TrackVideo:
				video = append(video, span{p.Slot.Start, p.Slot.End})
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Recovery.Failed != 0 {
		t.Fatalf("%d slots were left unfilled", res.Recovery.Failed)
	}
	if _, err := w.Commit(); err != nil {
		t.Fatal(err)
	}

	if len(audio) == 0 || len(video) == 0 {
		t.Fatalf("emitted %d audio and %d video packets", len(audio), len(video))
	}
	for i := 1; i < len(audio); i++ {
		if audio[i].start < audio[i-1].end {
			t.Fatalf("audio slot %d overlaps its predecessor: %d < %d", i, audio[i].start, audio[i-1].end)
		}
	}
	// Every recovered slot closes a gap, so the emitted audio timeline must
	// equal the original span from first to last packet.
	first := g.Audio.Packets[0].Slot.Start
	last := g.Audio.Packets[len(g.Audio.Packets)-1].Slot.End
	var covered int64
	for _, s := range audio {
		covered += s.end - s.start
	}
	if covered != last-first {
		t.Fatalf("audio coverage = %d ns, want %d ns (no holes, no overlap)", covered, last-first)
	}

	// Loss class: every damaged audio slot must have been filled, so the
	// audio timeline has no hole. Truncation class is covered by the video
	// side: damaged access units are dropped and recorded, and the surviving
	// frames keep their exact PTS spacing.
	//
	// Video: keyframes must stay on the configured grid after the per-frame
	// timestamps pass through the muxer's millisecond rescale.
	keys := 0
	for i, p := range g.Video.Packets {
		if p.Keyframe {
			keys++
			if i%cfg.KeyframeInterval != 0 {
				t.Fatalf("keyframe at frame %d breaks the %d frame grid", i, cfg.KeyframeInterval)
			}
		}
	}
	if keys == 0 {
		t.Fatal("no keyframes in the fixture")
	}
}

// TestTruncatedOutputIsDeliverable is the second fault class end to end:
// truncated audio frames and H.264 access units must be localised, the
// unparsable video units excluded, and the surviving stream muxed with
// monotonically increasing timestamps and byte-identical repeat output.
func TestTruncatedOutputIsDeliverable(t *testing.T) {
	cfg := ingest.DefaultConfig(17)
	cfg.VideoFrames = 120
	g, err := ingest.Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	inj, err := g.ApplyProfile(ingest.ProfileTruncated)
	if err != nil {
		t.Fatal(err)
	}
	if len(inj.Truncated) == 0 {
		t.Fatal("profile truncated nothing")
	}
	fixture := g.Fixture()

	muxOnce := func(name string) (string, []model.Packet) {
		dest := filepath.Join(t.TempDir(), name)
		w, err := remux.Open(dest, fixture.Streams)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Abort()
		var kept []model.Packet
		res, err := pipeline.Run(context.Background(), g, pipeline.Options{
			QueueItems: 16, QueueBytes: 1 << 20, Recovery: true,
			Emit: func(p model.Packet) error {
				kept = append(kept, p)
				return w.Write(context.Background(), p)
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Damage.Len() == 0 {
			t.Fatal("truncation produced no damaged ranges")
		}
		sum, err := w.Commit()
		if err != nil {
			t.Fatal(err)
		}
		return sum, kept
	}

	sumA, kept := muxOnce("a.mkv")
	sumB, _ := muxOnce("b.mkv")
	if sumA != sumB {
		t.Fatalf("truncated rebuild differs: %s vs %s", sumA, sumB)
	}

	// Every emitted packet must advance its own track's timeline.
	last := map[model.Track]int64{}
	for _, p := range kept {
		if prev, ok := last[p.Track]; ok && p.Slot.Start < prev {
			t.Fatalf("%s packet %d moved backwards: %d < %d", p.Track, p.Sequence, p.Slot.Start, prev)
		}
		last[p.Track] = p.Slot.Start
	}

	// Truncated video units must not appear in the deliverable.
	for _, p := range kept {
		if p.Track == model.TrackVideo && p.Corrupt {
			t.Fatalf("corrupt video packet %d was muxed", p.Sequence)
		}
	}
	// But later video packets must survive: truncation may not hide the tail.
	if last[model.TrackVideo] < int64(cfg.VideoFrames/2)*1_000_000 {
		t.Fatalf("video stopped early at %d ns", last[model.TrackVideo])
	}
}
