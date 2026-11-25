package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pion/rtp"

	"mediaqa/internal/fixture"
	"mediaqa/internal/manifest"
	"mediaqa/internal/media"
)

// setupWorkdir builds a work directory with a normal and a damaged
// asset plus a manifest referencing both.
func setupWorkdir(t *testing.T) (string, *manifest.Manifest) {
	t.Helper()
	dir := t.TempDir()

	normal, err := fixture.Gen(11, []fixture.TrackSpec{
		{ID: 1, Kind: "video", Timescale: 90000, Samples: videoSamples(0)},
		{ID: 2, Kind: "audio", Timescale: 90000, Samples: audioSamples(0)},
	})
	if err != nil {
		t.Fatal(err)
	}
	damaged, err := fixture.Gen(12, []fixture.TrackSpec{
		{ID: 1, Kind: "video", Timescale: 90000, Samples: videoSamples(0)},
	})
	if err != nil {
		t.Fatal(err)
	}
	damaged = fixture.Truncate(damaged, len(damaged)-100) // tail samples corrupt

	if err := os.WriteFile(filepath.Join(dir, "normal.mp4"), normal, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "damaged.mp4"), damaged, 0o644); err != nil {
		t.Fatal(err)
	}
	text := `{"track_id":"v1","path":"normal.mp4","kind":"video","timescale":90000,"edits":[{"media_start":0,"duration":30000}]}
{"track_id":"a1","path":"normal.mp4","kind":"audio","timescale":90000,"depends_on":["v1"],"edits":[{"media_start":0,"duration":30000}]}
{"track_id":"v2","path":"damaged.mp4","kind":"video","timescale":90000}
`
	m, err := manifest.Parse(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	return dir, m
}

func videoSamples(startDTS int64) []fixture.SampleSpec {
	var out []fixture.SampleSpec
	for i := 0; i < 10; i++ {
		out = append(out, fixture.SampleSpec{Duration: 3000, Key: i%5 == 0})
	}
	_ = startDTS
	return out
}

func audioSamples(startDTS int64) []fixture.SampleSpec {
	var out []fixture.SampleSpec
	for i := 0; i < 10; i++ {
		out = append(out, fixture.SampleSpec{Duration: 3000})
	}
	return out
}

func TestRunEndToEnd(t *testing.T) {
	dir, m := setupWorkdir(t)
	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := Run(m, Config{WorkDir: dir, OutDir: out, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.Errs.HasFatal() {
		t.Fatalf("fatal errors: %v", res.Errs.All())
	}
	if len(res.Tracks) != 3 {
		t.Fatalf("tracks=%d", len(res.Tracks))
	}
	// Damaged track: some samples invalid, recoverable errors registered.
	var v2 TrackResult
	for _, tr := range res.Tracks {
		if tr.TrackID == "v2" {
			v2 = tr
		}
	}
	if v2.InvalidCount == 0 {
		t.Fatal("expected invalid samples in damaged track")
	}
	if res.Errs.Count(media.SeverityRecoverable) == 0 {
		t.Fatal("expected recoverable errors")
	}
	// Outputs exist.
	for _, tr := range res.Tracks {
		if tr.RemuxPath == "" || tr.IndexPath == "" {
			t.Fatalf("track %s missing outputs", tr.TrackID)
		}
		if _, err := os.Stat(tr.RemuxPath); err != nil {
			t.Fatalf("remux: %v", err)
		}
		if _, err := os.Stat(tr.IndexPath); err != nil {
			t.Fatalf("index: %v", err)
		}
	}
}

func TestRunDeterministic(t *testing.T) {
	dir, m := setupWorkdir(t)
	run := func() map[string]string {
		out := filepath.Join(dir, "out")
		os.Mkdir(out, 0o755)
		res, err := Run(m, Config{WorkDir: dir, OutDir: out, Workers: 3})
		if err != nil {
			t.Fatal(err)
		}
		sums := map[string]string{}
		for _, tr := range res.Tracks {
			data, err := os.ReadFile(tr.IndexPath)
			if err != nil {
				t.Fatal(err)
			}
			sums[tr.TrackID] = string(data)
			remux, err := os.ReadFile(tr.RemuxPath)
			if err != nil {
				t.Fatal(err)
			}
			sums[tr.TrackID+".mp4"] = string(remux)
		}
		return sums
	}
	a := run()
	b := run()
	if len(a) != len(b) {
		t.Fatal("track count changed between runs")
	}
	for k, v := range a {
		if b[k] != v {
			t.Fatalf("run mismatch for %s", k)
		}
	}
}

func TestAVSyncZeroForAlignedTracks(t *testing.T) {
	dir, m := setupWorkdir(t)
	out := filepath.Join(dir, "out")
	os.Mkdir(out, 0o755)
	res, err := Run(m, Config{WorkDir: dir, OutDir: out})
	if err != nil {
		t.Fatal(err)
	}
	var v, a TrackResult
	for _, tr := range res.Tracks {
		switch tr.TrackID {
		case "v1":
			v = tr
		case "a1":
			a = tr
		}
	}
	if got := AVSyncMaxMS(v, a); got != 0 {
		t.Fatalf("av sync = %v ms", got)
	}
}

func TestRecoverRTPWritesBackToErrorModel(t *testing.T) {
	var errs media.List
	var received, cache []*rtp.Packet
	// Sequence 0..9 with 3 and 7 lost on the wire but cached.
	for s := uint16(0); s < 10; s++ {
		p := &rtp.Packet{Header: rtp.Header{SequenceNumber: s, Timestamp: uint32(s) * 3000}}
		if s == 3 || s == 7 {
			cache = append(cache, p)
			continue
		}
		received = append(received, p)
	}
	res := RecoverRTP("v1", 90000, received, cache, &errs)
	if res.Stats.Recovered != 2 || res.Stats.Lost != 0 {
		t.Fatalf("stats=%+v", res.Stats)
	}
	if len(res.Delivered) != 10 {
		t.Fatalf("delivered=%d", len(res.Delivered))
	}
	// Timestamps map onto the rational base: packet 4 -> 4*3000/90000 s.
	if got := res.Delivered[4].Timestamp; got != media.FromTicks(12000, 90000) {
		t.Fatalf("ts=%v", got)
	}
}
