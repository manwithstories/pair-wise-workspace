package index

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/media"
)

func testIndexProject(payload []byte) *media.Project {
	// Place two sync samples inside payload at known offsets.
	copy(payload[10:], SyncMagic)
	copy(payload[40:], SyncMagic)
	tr := &media.Track{ID: 9, Kind: media.KindVideo, Timescale: 90_000}
	tr.Samples = []media.Sample{
		{Index: 1, PTS: 0, Duration: 3000, Size: 16, Offset: 10, Sync: true},
		{Index: 2, PTS: 3000, Duration: 3000, Size: 16, Offset: 26, Sync: false},
		{Index: 3, PTS: 6000, Duration: 3000, Size: 16, Offset: 40, Sync: true},
	}
	return &media.Project{Tracks: []*media.Track{tr}}
}

func TestRebuildVerifyAndNearestSync(t *testing.T) {
	payload := make([]byte, 64)
	p := testIndexProject(payload)
	idx, faults := Rebuild(p)
	if len(faults) != 0 || !idx.Complete {
		t.Fatalf("rebuild faults=%#v complete=%v", faults, idx.Complete)
	}
	if len(idx.Tracks[9].Points) != 2 {
		t.Fatalf("points=%d", len(idx.Tracks[9].Points))
	}
	misses, vf := idx.Verify(bytes.NewReader(payload), int64(len(payload)))
	if misses != 0 || len(vf) != 0 {
		t.Fatalf("misses=%d faults=%#v", misses, vf)
	}
	pt, ok := idx.NearestSync(9, 2)
	if !ok || pt.SampleNumber != 1 {
		t.Fatalf("nearest sync = %#v %v", pt, ok)
	}
	pt, ok = idx.NearestSync(9, 3)
	if !ok || pt.SampleNumber != 3 {
		t.Fatalf("nearest sync at 3 = %#v %v", pt, ok)
	}
	path := filepath.Join(t.TempDir(), "index.json")
	if err := idx.WriteJSON(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyDetectsCorruptSeekPointAndRefusesPublish(t *testing.T) {
	payload := make([]byte, 64)
	p := testIndexProject(payload)
	idx, _ := Rebuild(p)
	payload[10] = 'X' // corrupt first sync marker
	misses, faults := idx.Verify(bytes.NewReader(payload), int64(len(payload)))
	if misses != 1 || len(faults) != 1 {
		t.Fatalf("misses=%d faults=%d", misses, len(faults))
	}
	if idx.Complete {
		t.Fatal("index must not remain complete after miss")
	}
	if err := idx.WriteJSON(filepath.Join(t.TempDir(), "bad.json")); err == nil {
		t.Fatal("expected refusal to publish incomplete index")
	}
	if _, ok := idx.NearestSync(9, 2); ok {
		t.Fatal("unverified point must not be returned as recovery anchor")
	}
}

func TestRebuildRejectsTrackWithoutSync(t *testing.T) {
	tr := &media.Track{ID: 1, Timescale: 1000, Samples: []media.Sample{{Index: 1, Offset: 0, Size: 8}}}
	idx, faults := Rebuild(&media.Project{Tracks: []*media.Track{tr}})
	if len(faults) == 0 || idx.Complete {
		t.Fatalf("expected missing sync fault, faults=%#v", faults)
	}
}
