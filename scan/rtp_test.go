package scan

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pion/rtp"
	"mediaslice/media"
)

// TestAmbiguousWrapStopsTrack writes an RTP sidecar whose timestamp moves
// backwards without an advancing sequence number (not a provable wrap) and
// asserts the scanner stops the track with a timestamp-wrap error while the
// MP4 parse itself remains valid.
func TestAmbiguousWrapStopsTrack(t *testing.T) {
	dir := t.TempDir()
	mp4Path, _, plan := buildFixture(t, dir, "800k", false)

	bad := filepath.Join(dir, "bad.rtp")
	f, err := os.Create(bad)
	if err != nil {
		t.Fatal(err)
	}
	hdr := make([]byte, 2)
	for i := 0; i < len(plan.Video); i++ {
		ts := uint32(100000 - i*3600) // monotonic downward, seq held constant
		if i > 2 {
			ts = uint32(50000) // sudden backward step, same sequence
		}
		pkt := rtp.Packet{Header: rtp.Header{
			Version: 2, PayloadType: 96, SequenceNumber: uint16(i % 2),
			Timestamp: ts, SSRC: 1,
		}, Payload: []byte{0, 0, 0, byte(i)}}
		raw, err := pkt.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		_ = hdr
		if _, err := f.Write([]byte{byte(len(raw) >> 8), byte(len(raw))}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(raw); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()

	idx, err := ScanFile(mp4Path, bad, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	e := idx.Errors.First(media.KindTimestampWrap)
	if e == nil {
		t.Fatalf("expected timestamp-wrap error, got %v", idx.Errors)
	}
	video := idx.VideoTrack()
	if video == nil || !video.Stopped {
		t.Fatal("track was not marked stopped")
	}
	// Sibling audio track results must survive.
	if len(idx.Tracks) < 2 || idx.TrackByID(2) == nil {
		t.Fatal("sibling track results lost")
	}
}

// TestIndexRoundTripPersistsColumns ensures the columnar tables survive a
// write/read index cycle.
func TestIndexRoundTripPersistsColumns(t *testing.T) {
	dir := t.TempDir()
	mp4Path, rtpPath, _ := buildFixture(t, dir, "800k", false)
	idx, err := ScanFile(mp4Path, rtpPath, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	path, err := WriteIndex(dir, idx)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := ReadIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.VideoTrack().SampleCount() != idx.VideoTrack().SampleCount() {
		t.Fatal("sample count changed across round trip")
	}
	if mm := VerifyIndex(reloaded, mp4Path); mm != nil {
		t.Fatalf("reloaded index failed verification: %v", mm)
	}
}
