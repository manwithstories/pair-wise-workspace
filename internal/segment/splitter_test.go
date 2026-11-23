package segment_test

import (
	"testing"

	"media-pipeline/internal/clock"
	"media-pipeline/internal/media"
	"media-pipeline/internal/segment"
)

var tb = clock.Timebase{SampleRate: 48000, Channels: 1, BitsPerSample: 16}

func block(id string, first, frames uint64) media.Block {
	return media.Block{
		ID: id, Format: tb, FirstSample: first,
		PCM: make([]byte, int(frames)*tb.FrameSize()),
	}
}

// 总帧数与覆盖区间必须连续无重叠、无缺口。
func assertCoverage(t *testing.T, segs []segment.Segment, totalFrames uint64) {
	t.Helper()
	var covered uint64
	for i, sg := range segs {
		if sg.EndSample-sg.FirstSample != sg.Frames {
			t.Fatalf("seg %d frame mismatch", i)
		}
		if i > 0 && sg.FirstSample != segs[i-1].EndSample {
			t.Fatalf("seg %d overlaps/gaps previous boundary", i)
		}
		covered += sg.Frames
	}
	if covered != totalFrames {
		t.Fatalf("covered %d frames, want %d (double counting?)", covered, totalFrames)
	}
}

func TestSplitByTargetDurationAcrossBlocks(t *testing.T) {
	// 同一段 PCM 用三种不同的块边界喂入，结果必须一致。
	feed := func(sizes ...uint64) []segment.Segment {
		s := segment.New(segment.Config{TargetFrames: 10, MinFrames: 1}, tb.FrameSize())
		var first uint64
		var got []segment.Segment
		for i, n := range sizes {
			got = append(got, mustPush(t, s, block(string(rune('a'+i)), first, n))...)
			first += n
		}
		got = append(got, s.Flush()...)
		return got
	}
	whole := feed(25)
	split := feed(7, 8, 10)
	choppy := feed(3, 3, 3, 3, 3, 3, 3, 4)

	if len(whole) != len(split) || len(split) != len(choppy) {
		t.Fatalf("segment counts differ: %d %d %d", len(whole), len(split), len(choppy))
	}
	for i := range whole {
		if whole[i].FirstSample != split[i].FirstSample ||
			split[i].FirstSample != choppy[i].FirstSample ||
			whole[i].Frames != split[i].Frames {
			t.Fatalf("seg %d differs across blockings: %+v %+v %+v",
				i, whole[i], split[i], choppy[i])
		}
	}
	assertCoverage(t, whole, 25)
	assertCoverage(t, split, 25)
}

func mustPush(t *testing.T, s *segment.Splitter, b media.Block) []segment.Segment {
	t.Helper()
	got, err := s.Push(b)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	return got
}

func TestSilenceBoundaryPreemptsTarget(t *testing.T) {
	sp := segment.New(segment.Config{TargetFrames: 20, MinFrames: 1}, tb.FrameSize())
	sp.AddSilenceBoundary(segment.SilenceCut{Sample: 12})
	got := mustPush(t, sp, block("a", 0, 25))
	got = append(got, sp.Flush()...)
	if len(got) != 2 {
		t.Fatalf("segments = %d, want 2", len(got))
	}
	if got[0].EndSample != 12 {
		t.Fatalf("silence cut not honored: first ends at %d, want 12", got[0].EndSample)
	}
	assertCoverage(t, got, 25)
}

func TestStaleSilenceCutIgnored(t *testing.T) {
	sp := segment.New(segment.Config{TargetFrames: 10, MinFrames: 1}, tb.FrameSize())
	sp.AddSilenceBoundary(segment.SilenceCut{Sample: 5})
	got := mustPush(t, sp, block("a", 0, 12))
	got = append(got, sp.Flush()...)
	// 切点 5 仍在第一段内，应产生 [0,5) 与 [5,...)
	if got[0].FirstSample != 0 || got[0].EndSample != 5 {
		t.Fatalf("first segment = %+v, want [0,5)", got[0])
	}
	assertCoverage(t, got, 12)
}

func TestGapsAttributedToOwningSegment(t *testing.T) {
	sp := segment.New(segment.Config{TargetFrames: 10, MinFrames: 1}, tb.FrameSize())
	b := block("a", 0, 20)
	b.Gaps = []media.Gap{{FirstSample: 12, Frames: 2, Reason: "rtp-burst-loss"}}
	var got []segment.Segment
	got = append(got, mustPush(t, sp, b)...)
	got = append(got, sp.Flush()...)
	if len(got) != 2 || len(got[1].Gaps) != 1 || len(got[0].Gaps) != 0 {
		t.Fatalf("gap attribution wrong: %+v", got)
	}
}
