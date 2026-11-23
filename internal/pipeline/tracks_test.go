package pipeline

import "testing"

// TestJitterBuffersEachCameraIndependently verifies that interleaved camera
// streams do not share one buffer. If they did, one camera's newest arrival
// would freeze another's intervals and the committed set would be wrong.
func TestJitterBuffersEachCameraIndependently(t *testing.T) {
	s, media, err := buildSample(Config{
		Seed: 21, Segments: 3, SegmentDurNS: 2_000_000_000, PacketRate: 50,
		Cameras: []string{"cam0", "cam1", "cam2"}, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	j := runJitter(s)
	if len(j.Cameras) != 3 {
		t.Fatalf("buffered cameras = %v, want 3", j.Cameras)
	}
	perTrack := map[string]int{}
	for _, iv := range j.Intervals {
		perTrack[iv.Track]++
	}
	// Each camera covers the same 6 seconds at 40ms granularity; every track
	// must show a comparable number of committed intervals.
	for _, cam := range j.Cameras {
		if perTrack[cam] < 3 {
			t.Fatalf("camera %s committed only %d intervals", cam, perTrack[cam])
		}
	}
	// The runtime spans 6 seconds; interleaved buffering would collapse to a
	// single track and lose the other cameras' intervals entirely.
	if len(perTrack) != 3 {
		t.Fatalf("committed tracks = %v, want 3 distinct", perTrack)
	}
	if len(media.Segments) != 9 {
		t.Fatalf("segments = %d, want 9", len(media.Segments))
	}
}
