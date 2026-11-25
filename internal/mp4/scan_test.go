package mp4scan_test

import (
	"os"
	"path/filepath"
	"testing"

	mp4scan "mediaqa/internal/mp4"
	"mediaqa/internal/fixture"
	"mediaqa/internal/media"
)

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f.mp4")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func spec() []fixture.TrackSpec {
	return []fixture.TrackSpec{{
		ID: 1, Kind: "video", Timescale: 90000,
		Samples: []fixture.SampleSpec{
			{Duration: 3000, Key: true, CTOffset: 6000},
			{Duration: 3000, CTOffset: -3000},
			{Duration: 3000, CTOffset: -1500},
			{Duration: 3000, Key: true},
		},
	}}
}

func TestScanValid(t *testing.T) {
	data, err := fixture.Gen(7, spec())
	if err != nil {
		t.Fatal(err)
	}
	var errs media.List
	res, err := mp4scan.Scan(writeTemp(t, data), &errs)
	if err != nil {
		t.Fatal(err)
	}
	if !res.MoovFound || len(res.Tracks) != 1 {
		t.Fatalf("result: %+v", res)
	}
	tr := res.Tracks[0]
	if tr.Kind != "video" || tr.Timescale != 90000 || len(tr.Samples) != 4 {
		t.Fatalf("track: %+v", tr)
	}
	s := tr.Samples
	// DTS accumulates; PTS = DTS + ctts.
	if s[0].DTS != 0 || s[1].DTS != 3000 || s[3].DTS != 9000 {
		t.Fatalf("dts: %+v", s)
	}
	if s[0].PTS != 6000 || s[1].PTS != 0 || s[2].PTS != 4500 {
		t.Fatalf("pts: %d %d %d", s[0].PTS, s[1].PTS, s[2].PTS)
	}
	if !s[0].Key || s[1].Key || !s[3].Key {
		t.Fatalf("keys: %+v", s)
	}
	for _, sm := range s {
		if sm.Corrupt {
			t.Fatalf("unexpected corrupt sample %+v", sm)
		}
		if sm.Offset >= uint64(len(data)) || sm.Offset+uint64(sm.Size) > uint64(len(data)) {
			t.Fatalf("bad offset %+v", sm)
		}
	}
	if errs.Len() != 0 {
		t.Fatalf("errors: %v", errs.All())
	}
}

func TestScanTruncatedMdat(t *testing.T) {
	data, _ := fixture.Gen(7, spec())
	cut := len(data) - 50 // inside the last sample(s)
	var errs media.List
	res, err := mp4scan.Scan(writeTemp(t, fixture.Truncate(data, cut)), &errs)
	if err != nil {
		t.Fatal(err)
	}
	tr := res.Tracks[0]
	var corrupt, ok int
	for _, s := range tr.Samples {
		if s.Corrupt {
			corrupt++
		} else {
			ok++
		}
	}
	if corrupt == 0 || ok == 0 {
		t.Fatalf("corrupt=%d ok=%d", corrupt, ok)
	}
	if errs.Count(media.SeverityRecoverable) == 0 {
		t.Fatal("expected recoverable error for corrupt interval")
	}
}

func TestScanMissingMoov(t *testing.T) {
	data, _ := fixture.Gen(7, spec())
	var errs media.List
	res, err := mp4scan.Scan(writeTemp(t, fixture.StripMoov(data)), &errs)
	if err != nil {
		t.Fatal(err)
	}
	if res.MoovFound {
		t.Fatal("moov should be absent")
	}
	found := false
	for _, e := range errs.All() {
		if e.Code == "missing-moov" {
			found = true
		}
	}
	if !found {
		t.Fatalf("errors: %v", errs.All())
	}
}
