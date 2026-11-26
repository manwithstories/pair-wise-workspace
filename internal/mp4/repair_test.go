package mp4

import (
	"bytes"
	"testing"

	"mediad/internal/media"
)

func testProject() *media.Project {
	video := &media.Track{ID: 1, Kind: media.KindVideo, Timescale: 90_000, Width: 1280, Height: 720}
	for i := uint32(0); i < 6; i++ {
		video.Samples = append(video.Samples, media.Sample{
			Index: i + 1, PTS: int64(i) * 3000, DTS: int64(i) * 3000, Duration: 3000,
			Size: 24, Sync: i%2 == 0,
		})
	}
	audio := &media.Track{ID: 2, Kind: media.KindAudio, Timescale: 48_000}
	for i := uint32(0); i < 4; i++ {
		audio.Samples = append(audio.Samples, media.Sample{
			Index: i + 1, PTS: int64(i) * 1024, DTS: int64(i) * 1024, Duration: 1024, Size: 16, Sync: true,
		})
	}
	return &media.Project{MovieTimescale: 90_000, Tracks: []*media.Track{video, audio}, SourceFormat: "mp4"}
}

func TestBuildAndReadRoundTrip(t *testing.T) {
	p := testProject()
	data, err := Build(p)
	if err != nil {
		t.Fatal(err)
	}
	got, boxes, faults, err := ReadProject(bytes.NewReader(data), int64(len(data)), false)
	if err != nil {
		t.Fatalf("ReadProject: %v", err)
	}
	if len(faults) != 0 {
		t.Fatalf("unexpected faults: %#v", faults)
	}
	if len(boxes) < 3 {
		t.Fatalf("top boxes = %d", len(boxes))
	}
	if len(got.Tracks) != 2 {
		t.Fatalf("tracks=%d", len(got.Tracks))
	}
	if got.Tracks[0].ID != 1 || got.Tracks[0].Timescale != 90_000 || len(got.Tracks[0].Samples) != 6 {
		t.Fatalf("video track mismatch: %#v", got.Tracks[0])
	}
	if got.Tracks[1].ID != 2 || len(got.Tracks[1].Samples) != 4 {
		t.Fatalf("audio track mismatch: %#v", got.Tracks[1])
	}
	for _, tr := range got.Tracks {
		for _, s := range tr.Samples {
			if s.Offset < 0 {
				t.Fatalf("missing offset sample %d/%d", tr.ID, s.Index)
			}
		}
	}
	if nums := got.Tracks[0].SyncSampleNumbers(); len(nums) != 3 || nums[0] != 1 || nums[1] != 3 || nums[2] != 5 {
		t.Fatalf("sync numbers = %v", nums)
	}
}

func TestRepairTimetablesFixesRegressionAndMissingDuration(t *testing.T) {
	p := testProject()
	v := p.Tracks[0]
	v.Samples[2].PTS = v.Samples[1].PTS - 500
	v.Samples[3].Duration = 0
	rep := RepairTimetables(p)
	if rep.TimestampFixes < 2 {
		t.Fatalf("repair report = %#v", rep)
	}
	for i := 1; i < len(v.Samples); i++ {
		if v.Samples[i].DTS <= v.Samples[i-1].DTS && v.Samples[i].Duration == 0 {
			t.Fatal("dts not monotonic")
		}
		if v.Samples[i].PTS < v.Samples[i-1].PTS {
			t.Fatal("pts still regresses")
		}
	}
	if v.Samples[3].Duration != 3000 {
		t.Fatalf("default duration = %d", v.Samples[3].Duration)
	}
	if _, err := Build(p); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeTimestampsHandlesMissingBackwardAndWrap(t *testing.T) {
	window := uint64(1) << 32
	vals := []*int64{ptr(int64(window) - 8), ptr(4), nil, ptr(100), ptr(90)}
	got, faults := NormalizeTimestamps(vals, window, 3000)
	if len(faults) < 3 {
		t.Fatalf("faults=%#v", faults)
	}
	if got[1] <= got[0] {
		t.Fatalf("wrap not unwrapped: %v", got)
	}
	if got[2] != got[1]+3000 {
		t.Fatalf("missing fill = %v", got)
	}
	if got[4] <= got[3] {
		t.Fatalf("backward not repaired: %v", got)
	}
}

func ptr(v int64) *int64 { return &v }
