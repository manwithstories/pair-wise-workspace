package mapping

import (
	"path/filepath"
	"testing"

	"relay/internal/media"
)

func writeTempMap(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "map.json")
	if err := writeFile(p, []byte(body)); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolveAppliesClockOffsetAndTrack(t *testing.T) {
	cfg := &Config{Version: 1, Cameras: map[string]CameraConfig{
		"7": {TrackID: 100, ClockOffsetNS: int64(50 * media.Millisecond)},
		"8": {TrackID: 200, ClockOffsetNS: int64(-10 * media.Millisecond)},
	}}
	m, err := NewMapper(cfg)
	if err != nil {
		t.Fatal(err)
	}
	track, ns, err := m.Resolve(&media.Frame{CameraID: 7, EventNS: 1000, Sequence: 1})
	if err != nil || track != 100 || ns != media.TimeNS(50_001_000) {
		t.Fatalf("resolve cam7: %d %d %v", track, ns, err)
	}
	track, ns, err = m.Resolve(&media.Frame{CameraID: 8, EventNS: 1000})
	if err != nil || track != 200 || ns != media.TimeNS(-9_999_000) {
		t.Fatalf("resolve cam8: %d %d %v", track, ns, err)
	}
}

func TestResolveRejectsUnknownCamera(t *testing.T) {
	m, _ := NewMapper(&Config{Version: 1, Cameras: map[string]CameraConfig{
		"1": {TrackID: 10},
	}})
	if _, _, err := m.Resolve(&media.Frame{CameraID: 99}); err == nil {
		t.Fatal("unknown camera must error")
	}
}

func TestConfigValidation(t *testing.T) {
	bad := []Config{
		{Version: 2, Cameras: map[string]CameraConfig{"1": {TrackID: 1}}},
		{Version: 1, Cameras: map[string]CameraConfig{}},
		{Version: 1, Cameras: map[string]CameraConfig{
			"1": {TrackID: 5}, "2": {TrackID: 5},
		}},
		{Version: 1, Cameras: map[string]CameraConfig{"1": {TrackID: 0}}},
	}
	for i, c := range bad {
		if err := c.Validate(); err == nil {
			t.Fatalf("case %d should be invalid", i)
		}
	}
}

func TestLoadConfigRoundTrip(t *testing.T) {
	cfg := &Config{Version: 1, Cameras: map[string]CameraConfig{
		"1": {TrackID: 10, ClockOffsetNS: 100},
	}}
	raw, _ := cfg.MarshalJSON()
	p := writeTempMap(t, string(raw))
	got, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Cameras["1"].TrackID != 10 {
		t.Fatalf("round trip lost data: %+v", got)
	}
}

func TestDedupAcceptDuplicateConflict(t *testing.T) {
	d := NewDedup()
	f1 := &media.Frame{Sequence: 1, SHA256: media.SumSHA256([]byte("a"))}
	f1dup := &media.Frame{Sequence: 1, SHA256: media.SumSHA256([]byte("a"))}
	f1conf := &media.Frame{Sequence: 1, SHA256: media.SumSHA256([]byte("b"))}
	f2 := &media.Frame{Sequence: 2, SHA256: media.SumSHA256([]byte("c"))}

	if d.Classify(1, f1).Kind != DecisionAccept {
		t.Fatal("first frame must accept")
	}
	if d.Classify(1, f1dup).Kind != DecisionDuplicate {
		t.Fatal("identical hash must be idempotent duplicate")
	}
	conf := d.Classify(1, f1conf)
	if conf.Kind != DecisionConflict || conf.Existing != f1.SHA256 {
		t.Fatalf("different hash must conflict: %+v", conf)
	}
	if d.Classify(1, f2).Kind != DecisionAccept {
		t.Fatal("new sequence must accept")
	}
	// 不同轨道的相同序号互不影响。
	if d.Classify(2, f1).Kind != DecisionAccept {
		t.Fatal("separate tracks must have independent ledgers")
	}
}

func TestDedupSnapshotRestore(t *testing.T) {
	d := NewDedup()
	d.Classify(3, &media.Frame{Sequence: 9, SHA256: media.SumSHA256([]byte("x"))})
	d2 := NewDedup()
	d2.Restore(d.Snapshot())
	dup := d2.Classify(3, &media.Frame{Sequence: 9, SHA256: media.SumSHA256([]byte("x"))})
	if dup.Kind != DecisionDuplicate {
		t.Fatalf("restored ledger failed: %s", dup.Kind)
	}
}
