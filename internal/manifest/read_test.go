package manifest

import (
	"fmt"
	"strings"
	"testing"

	merr "mediaroute/internal/errors"
)

const goodManifest = `{
  "version": 1,
  "tracks": [
    {"id": 1, "kind": "video", "source": "cam-a.mp4", "required": true, "origin_ms": 1234,
     "edits": [{"segment_duration_ms": 1000, "media_time_ms": 0}]},
    {"id": 2, "kind": "audio", "source": "mic-a.mp4", "required": true, "origin_ms": 1234,
     "audio": {"sample_rate": 44100, "channel_count": 2, "layout": "stereo"}}
  ]
}`

func TestReadValidManifest(t *testing.T) {
	m, err := Read(strings.NewReader(goodManifest))
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(m.Tracks) != 2 {
		t.Fatalf("want 2 tracks got %d", len(m.Tracks))
	}
	if len(m.Digest) != 64 {
		t.Fatalf("want sha256 digest got %q", m.Digest)
	}
	if got := m.TrackOffset(2); got <= m.TrackOffset(1) {
		t.Fatalf("track offsets must increase: %d then %d", m.TrackOffset(1), got)
	}
	if ids := m.RequiredIDs(); len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Fatalf("unexpected required ids %v", ids)
	}
}

func TestReadIsDeterministic(t *testing.T) {
	a, err := Read(strings.NewReader(goodManifest))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Read(strings.NewReader(goodManifest))
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest != b.Digest {
		t.Fatalf("digest must be stable: %s vs %s", a.Digest, b.Digest)
	}
}

func TestMissingTrackReportsOffset(t *testing.T) {
	bad := `{"version":1,"tracks":[{"id":1,"kind":"video","source":"a.mp4","required":false}]}`
	_, err := Read(strings.NewReader(bad))
	if err == nil {
		t.Fatal("want error for manifest with no required tracks")
	}
	e, ok := merr.As(err)
	if !ok || e.Kind != merr.KindManifest {
		t.Fatalf("want manifest error, got %v", err)
	}
	if e.ByteOffset < 0 {
		t.Fatal("manifest error must carry a byte offset")
	}
}

func TestDuplicateTrackReference(t *testing.T) {
	bad := `{"version":1,"tracks":[
	  {"id":1,"kind":"video","source":"same.mp4","required":true},
	  {"id":2,"kind":"audio","source":"same.mp4","required":true}]}`
	_, err := Read(strings.NewReader(bad))
	e, ok := merr.As(err)
	if !ok || e.Kind != merr.KindManifest {
		t.Fatalf("want manifest error got %v", err)
	}
	if e.TrackID != 2 {
		t.Fatalf("want offending track 2 got %d", e.TrackID)
	}
	if e.ByteOffset <= 0 {
		t.Fatalf("want positive byte offset got %d", e.ByteOffset)
	}
}

func TestNegativeDurationReported(t *testing.T) {
	bad := `{"version":1,"tracks":[
	  {"id":7,"kind":"audio","source":"a.m4a","required":true,
	   "edits":[{"segment_duration_ms":-5,"media_time_ms":0}]}]}`
	_, err := Read(strings.NewReader(bad))
	e, ok := merr.As(err)
	if !ok || e.Kind != merr.KindManifest {
		t.Fatalf("want manifest error got %v", err)
	}
	if e.TrackID != 7 {
		t.Fatalf("want track 7 got %d", e.TrackID)
	}
	if !strings.Contains(e.Message, "negative edit duration") {
		t.Fatalf("unexpected message %q", e.Message)
	}
	if e.ByteOffset < 0 {
		t.Fatalf("want byte offset got %d", e.ByteOffset)
	}
}

func TestTooManyTracks(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"version":1,"tracks":[`)
	for i := 0; i < MaxTrackCount+1; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":%d,"kind":"video","source":"x%d.mp4","required":true}`, i+1, i)
	}
	b.WriteString(`]}`)
	_, err := Read(strings.NewReader(b.String()))
	if err == nil {
		t.Fatal("want limit error")
	}
	e, ok := merr.As(err)
	if !ok || !strings.Contains(e.Message, "track count exceeds") {
		t.Fatalf("want track count limit error, got %v", err)
	}
}

func TestInvalidAudioSpec(t *testing.T) {
	bad := `{"version":1,"tracks":[
	  {"id":3,"kind":"audio","source":"a.m4a","required":true,
	   "audio":{"sample_rate":0,"channel_count":2,"layout":"stereo"}}]}`
	_, err := Read(strings.NewReader(bad))
	e, ok := merr.As(err)
	if !ok || e.TrackID != 3 {
		t.Fatalf("want track 3 error got %v", err)
	}
}
