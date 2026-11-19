package media

import (
	"testing"
)

func buildVideo(t *testing.T, samples int) []byte {
	t.Helper()
	spec := TrackSpec{ID: 1, Kind: "video", Timescale: 90000, Codec: "h264", Width: 640, Height: 360}
	for i := 0; i < samples; i++ {
		spec.Samples = append(spec.Samples, SampleSpec{Duration: 3000, Size: 100, Sync: i%10 == 0})
	}
	spec.Payload = make([]byte, samples*100)
	data, err := BuildMP4([]TrackSpec{spec})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestBuildAndParseVideo(t *testing.T) {
	data := buildVideo(t, 30)
	pf, err := ParseFile(Reader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(pf.Tracks) != 1 {
		t.Fatalf("tracks=%d", len(pf.Tracks))
	}
	tr := pf.Tracks[0]
	if tr.ID != 1 || tr.Kind != "video" || tr.Timescale != 90000 {
		t.Fatalf("track=%+v", tr)
	}
	if len(tr.PTS) != 30 {
		t.Fatalf("samples=%d", len(tr.PTS))
	}
	if tr.PTS[1] != 33_333_333 {
		t.Fatalf("pts[1]=%d", tr.PTS[1])
	}
	if len(tr.Keyframes) != 3 {
		t.Fatalf("keyframes=%v", tr.Keyframes)
	}
	if tr.Keyframes[1] != 10 || tr.Keyframes[2] != 20 {
		t.Fatalf("keyframes=%v", tr.Keyframes)
	}
	if len(pf.Data) != 3000 {
		t.Fatalf("mdat payload=%d", len(pf.Data))
	}
	if pf.ChunkOffset[1] == 0 {
		t.Fatal("chunk offset not patched")
	}
}

func TestBuildAudioTrack(t *testing.T) {
	spec := TrackSpec{ID: 2, Kind: "audio", Timescale: 48000, Codec: "aac", Channels: 2}
	for i := 0; i < 100; i++ {
		spec.Samples = append(spec.Samples, SampleSpec{Duration: 1024, Size: 50, Sync: true})
	}
	spec.Payload = make([]byte, 5000)
	data, err := BuildMP4([]TrackSpec{spec})
	if err != nil {
		t.Fatal(err)
	}
	pf, err := ParseFile(Reader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	tr := pf.Tracks[0]
	if tr.Kind != "audio" {
		t.Fatalf("kind=%s", tr.Kind)
	}
	if len(tr.Keyframes) != 100 {
		t.Fatalf("audio track without stss should be all-sync: %d", len(tr.Keyframes))
	}
}

func TestBuildMultiTrackOffsets(t *testing.T) {
	v := TrackSpec{ID: 1, Kind: "video", Timescale: 90000, Codec: "h264", Width: 320, Height: 180}
	for i := 0; i < 5; i++ {
		v.Samples = append(v.Samples, SampleSpec{Duration: 3000, Size: 10, Sync: i == 0})
	}
	v.Payload = make([]byte, 50)
	a := TrackSpec{ID: 2, Kind: "audio", Timescale: 48000, Codec: "aac", Channels: 2}
	for i := 0; i < 5; i++ {
		a.Samples = append(a.Samples, SampleSpec{Duration: 1024, Size: 20, Sync: true})
	}
	a.Payload = make([]byte, 100)
	data, err := BuildMP4([]TrackSpec{v, a})
	if err != nil {
		t.Fatal(err)
	}
	pf, err := ParseFile(Reader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(pf.Tracks) != 2 {
		t.Fatalf("tracks=%d", len(pf.Tracks))
	}
	if pf.ChunkOffset[1] == pf.ChunkOffset[2] {
		t.Fatal("chunk offsets collide")
	}
	// Track 2's payload begins where track 1's ends.
	if pf.ChunkOffset[2]-pf.ChunkOffset[1] != 50 {
		t.Fatalf("offsets %d and %d not adjacent for 50-byte track", pf.ChunkOffset[1], pf.ChunkOffset[2])
	}
}

func TestBuildRejectsSizeMismatch(t *testing.T) {
	spec := TrackSpec{ID: 1, Kind: "video", Timescale: 90000, Codec: "h264",
		Samples: []SampleSpec{{Duration: 1, Size: 100, Sync: true}}, Payload: make([]byte, 50)}
	if _, err := BuildMP4([]TrackSpec{spec}); err == nil {
		t.Fatal("size mismatch accepted")
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	garbage := make([]byte, 256)
	garbage[0] = 0xff
	if _, err := ParseFile(Reader(garbage), int64(len(garbage))); err == nil {
		t.Fatal("garbage accepted as MP4")
	}
}

func TestParseRejectsOversizeFile(t *testing.T) {
	if _, err := ParseFile(Reader(nil), MaxFileBytes+1); err == nil {
		t.Fatal("oversized file accepted")
	}
}

func TestDeterministicOutput(t *testing.T) {
	a := buildVideo(t, 12)
	b := buildVideo(t, 12)
	if len(a) != len(b) {
		t.Fatalf("lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("byte %d differs", i)
		}
	}
}
