package seek

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"mp4probe/internal/index"
	"mp4probe/internal/moov"
	"mp4probe/internal/timeline"
)

// makeTrack builds a 100-sample track, delta 3600, sync every 20,
// sample i is 4 bytes encoding i, laid out consecutively from off 0.
func makeTrack() (*index.TrackIndex, []byte) {
	n := 100
	tr := &index.TrackIndex{
		TrackID: 1, Timescale: 90000, MediaDuration: 360000,
		SampleCount: n, SyncSampleCount: 5, ChunkCount: 5,
	}
	var data []byte
	for i := 0; i < n; i++ {
		tr.Offsets = append(tr.Offsets, int64(len(data)))
		tr.Sizes = append(tr.Sizes, 4)
		tr.DTS = append(tr.DTS, int64(i)*3600)
		tr.PTS = append(tr.PTS, int64(i)*3600)
		data = append(data, byte(i), byte(i>>8), 0xAA, 0x55)
	}
	for i := 0; i < n; i += 20 {
		tr.Sync = append(tr.Sync, uint32(i))
	}
	idx := &index.Index{Tracks: []*index.TrackIndex{tr}}
	_ = idx
	return tr, data
}

func locatorFor(tr *index.TrackIndex, data []byte, elst []moov.ElstEntry) *Locator {
	idx := &index.Index{Tracks: []*index.TrackIndex{tr}}
	l, err := NewLocator(idx, 1, timeline.NewMapper(elst, 90000, 90000, false), bytes.NewReader(data))
	if err != nil {
		panic(err)
	}
	return l
}

func shaOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestSeekBasic(t *testing.T) {
	tr, data := makeTrack()
	l := locatorFor(tr, data, nil)
	// Target sample 47 (pts 47*3600): landing sync = 40.
	res, err := l.Locate(47*3600, 8)
	if err != nil {
		t.Fatal(err)
	}
	if res.LandingSampleIndex != 40 || res.DecodePathLength != 8 {
		t.Fatalf("landing=%d path=%d", res.LandingSampleIndex, res.DecodePathLength)
	}
	if res.FrameType != "sync" {
		t.Fatalf("frame_type=%s", res.FrameType)
	}
	if len(res.Frames) != 8 || res.Frames[0].SampleIndex != 40 {
		t.Fatalf("frames=%v", res.Frames)
	}
	want := shaOf(data[40*4 : 41*4])
	if res.SHA256 != want || res.Frames[0].SHA256 != want {
		t.Fatalf("hash mismatch: %s != %s", res.SHA256, want)
	}
	if res.Frames[1].FrameType != "non-sync" {
		t.Fatalf("frame 41 should be non-sync")
	}
}

func TestSeekErrors(t *testing.T) {
	tr, data := makeTrack()
	// Beyond duration.
	if _, err := locatorFor(tr, data, nil).Locate(360000+1, 1); !errors.Is(err, ErrLocate) {
		t.Fatalf("beyond duration: %v", err)
	}
	// Empty stss.
	empty := *tr
	empty.AllSync = false
	empty.Sync = nil
	if _, err := locatorFor(&empty, data, nil).Locate(100, 1); !errors.Is(err, ErrLocate) {
		t.Fatalf("empty stss: %v", err)
	}
	// Before first sync: move first sync to sample 10.
	late := *tr
	late.Sync = []uint32{10, 20, 40, 60, 80}
	if _, err := locatorFor(&late, data, nil).Locate(5*3600, 1); !errors.Is(err, ErrLocate) {
		t.Fatalf("before first sync: %v", err)
	}
	// Single-sample track.
	single := *tr
	single.SampleCount = 1
	if _, err := locatorFor(&single, data, nil).Locate(0, 1); !errors.Is(err, ErrLocate) {
		t.Fatalf("single sample: %v", err)
	}
	// Zero-duration track.
	zero := *tr
	zero.MediaDuration = 0
	if _, err := locatorFor(&zero, data, nil).Locate(0, 1); !errors.Is(err, ErrLocate) {
		t.Fatalf("zero duration: %v", err)
	}
}

func TestSeekBFrameOrder(t *testing.T) {
	tr, data := makeTrack()
	// Give samples 40..43 B-frame style pts: decode 40(I,pts+3),
	// 41(B,pts+1), 42(B,pts+2), 43(P,pts+4) in 3600 units.
	base := int64(40 * 3600)
	tr.PTS[40] = base + 3*3600
	tr.PTS[41] = base + 1*3600
	tr.PTS[42] = base + 2*3600
	tr.PTS[43] = base + 4*3600
	l := locatorFor(tr, data, nil)
	// Aim at presentation pts of sample 42 (base+2*3600).
	res, err := l.Locate(base+2*3600, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Landing is sync sample 40; decode path covers 40,41,42.
	if res.LandingSampleIndex != 40 || res.DecodePathLength != 3 {
		t.Fatalf("landing=%d path=%d", res.LandingSampleIndex, res.DecodePathLength)
	}
}

func TestSeekEmptyEditSnap(t *testing.T) {
	tr, data := makeTrack()
	elst := []moov.ElstEntry{
		{Duration: 180000, MediaTime: -1, Rate: 1 << 16}, // 2s dwell
		{Duration: 360000, MediaTime: 0, Rate: 1 << 16},
	}
	l := locatorFor(tr, data, elst)
	res, err := l.Locate(90000, 2) // inside the empty edit
	if err != nil {
		t.Fatal(err)
	}
	if !res.SnappedFromEmptyEdit || res.LandingSampleIndex != 0 {
		t.Fatalf("snap=%v landing=%d", res.SnappedFromEmptyEdit, res.LandingSampleIndex)
	}
}

func TestHashSampleShortRead(t *testing.T) {
	r := bytes.NewReader([]byte{1, 2, 3})
	if _, err := HashSample(r, 0, 10); err == nil {
		t.Fatal("expected short read error")
	}
}

func TestSeekTruncatedFrames(t *testing.T) {
	tr, data := makeTrack()
	l := locatorFor(tr, data, nil)
	res, err := l.Locate(99*3600, 25) // landing=80, only 20 frames remain
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || len(res.Frames) != 20 {
		t.Fatalf("truncated=%v frames=%d", res.Truncated, len(res.Frames))
	}
}
