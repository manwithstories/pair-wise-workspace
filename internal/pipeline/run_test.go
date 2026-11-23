package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"archive.local/archive/internal/cache"
	"archive.local/archive/internal/fixture"
	"archive.local/archive/internal/model"
)

// buildWorkspace writes the same fixture family the demo uses, at small scale.
func buildWorkspace(t *testing.T, damage bool) string {
	return buildWorkspaceWith(t, damage, false)
}

// buildWorkspaceWith optionally plants a polluted cache entry, modelling the
// cache-pollution fixture family.
func buildWorkspaceWith(t *testing.T, damage, pollute bool) string {
	t.Helper()
	work := t.TempDir()
	secs := int64(120)
	videoSamples := 1200
	pad := int64(0)
	if damage {
		// Generate, then truncate at a byte inside the video payload.
		pad = 0
	}
	lay, err := fixture.Generate(filepath.Join(work, "reels", "feature.mp4"), fixture.Clip{
		TrackID: 1, Timescale: 90000, Origin: 90_000 * secs, Samples: videoSamples,
		Delta: 3600, KeyEvery: 25, Kind: "video", Payload: 2,
		ChunkSamples: 25, ConstSize: true, PadBeforeMdat: int(pad),
	})
	if err != nil {
		t.Fatal(err)
	}
	if damage {
		// Cut inside the payload so the tail samples are unrecoverable.
		cut := int64(lay.Offsets[videoSamples/2]) + 1
		if err := os.Truncate(filepath.Join(work, "reels", "feature.mp4"), cut); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fixture.Generate(filepath.Join(work, "reels", "feature.audio.mp4"), fixture.Clip{
		TrackID: 2, Timescale: 48000, Origin: 48_000 * secs, Samples: 600,
		Delta: 1024, KeyEvery: 1, Kind: "audio", Payload: 4,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Generate(filepath.Join(work, "reels", "feature.slate.mp4"), fixture.Clip{
		TrackID: 3, Timescale: 1000, Origin: 1000 * secs, Samples: 200,
		Delta: 2000, KeyEvery: 10, Kind: "video", Payload: 4,
	}); err != nil {
		t.Fatal(err)
	}
	span := int64(videoSamples) * 3600
	manifest := `track 1 reels/feature.mp4 90000 ` + itoa(90_000*secs) + ` video ` + itoa(90_000*secs+span) + "\n" +
		`track 2 reels/feature.audio.mp4 48000 ` + itoa(48_000*secs) + ` audio ` + itoa(48_000*secs+600*1024) + "\n" +
		`track 3 reels/feature.slate.mp4 1000 ` + itoa(1000*secs) + ` data ` + itoa(1000*secs+200*2000) + "\n" +
		`edit 1 ` + itoa(90_000*secs) + ` ` + itoa(span) + " 1 1\n" +
		`edit 2 -1 48000 1 1` + "\n" +
		`edit 2 ` + itoa(48_000*secs) + ` ` + itoa(600*1024) + " 1 1\n"
	if err := os.WriteFile(filepath.Join(work, "material.txt"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.GenerateTrace(filepath.Join(work, "transport.trace"), fixture.TracePlan{
		Packets: 800, Payload: 64, DupEvery: 31, ReorderEvery: 47,
		Bursts:   []fixture.Burst{{At: 200, Count: 5}, {At: 600, Count: 8}},
		StartSeq: 65350,
	}, 41); err != nil {
		t.Fatal(err)
	}
	if pollute {
		plantPollution(t, work)
	}
	return work
}

// plantPollution seeds a corrupted entry whose metadata claims a valid digest,
// modelling a cache store damaged by an earlier interrupted transfer.
func plantPollution(t *testing.T, work string) {
	t.Helper()
	store, err := cache.NewStore(filepath.Join(work, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	good := []byte("stale-cache-entry")
	src := cache.Provenance(strings.Repeat("0", 64), 99, 0, 0, uint64(len(good)))
	e, err := store.Put(good, src)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := store.Poison(e.Address); err != nil || !ok {
		t.Fatalf("poison: %v ok=%v", err, ok)
	}
}

func itoa(v int64) string { return fmtInt(v) }

func fmtInt(v int64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

func run(t *testing.T, work string, resume bool) (*Result, error) {
	t.Helper()
	return Run(Options{WorkDir: work, ManifestPath: "material.txt",
		TracePath: filepath.Join(work, "transport.trace"), Resume: resume,
		Duration: time.Minute})
}

func TestHealthRunIsClean(t *testing.T) {
	work := buildWorkspace(t, false)
	res, err := run(t, work, false)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != OK {
		t.Fatalf("status = %v defect = %v", res.Status, res.Defect)
	}
	if res.IndexDigest == "" || len(res.IndexDigest) != 64 {
		t.Fatalf("digest = %q", res.IndexDigest)
	}
	if len(res.Tracks) != 3 {
		t.Fatalf("tracks = %d", len(res.Tracks))
	}
	if res.Transport.Duplicate == 0 || res.Transport.Reordered == 0 || res.Transport.BurstLoss == 0 {
		t.Fatalf("transport = %+v", res.Transport)
	}
}

func TestDamagedRunDiagnosesAndStillIndexes(t *testing.T) {
	work := buildWorkspace(t, true)
	res, _ := run(t, work, false)
	if res == nil {
		t.Fatal("nil result")
	}
	if res.Defect == nil || res.Defect.Code != model.CodeTruncatedSample {
		t.Fatalf("defect = %+v", res.Defect)
	}
	if res.Status != Damaged {
		t.Fatalf("status = %v", res.Status)
	}
	if res.DamageOffset <= 0 {
		t.Fatalf("damage offset = %d", res.DamageOffset)
	}
	if res.RecoveredSamples <= 0 {
		t.Fatalf("recovered = %d", res.RecoveredSamples)
	}
	if res.IndexDigest == "" {
		t.Fatal("no index built over the damaged range")
	}
	if res.Defect == nil || res.Defect.Recoverable == nil {
		t.Fatalf("defect = %+v", res.Defect)
	}
}

func TestResumeUsesValidatedCheckpoint(t *testing.T) {
	work := buildWorkspace(t, true)
	// First run leaves a checkpoint; resume must validate and report it.
	if _, err := run(t, work, false); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	res, err := run(t, work, true)
	if err != nil || res == nil {
		t.Fatalf("resume err = %v res=%v", err, res)
	}
	if res.Defect == nil || res.Defect.Code != model.CodeTruncatedSample {
		t.Fatalf("resume defect = %+v", res.Defect)
	}
	if !res.ResumeUsed {
		t.Fatal("checkpoint was not used")
	}
	if res.ResumeMs > 2000 {
		t.Fatalf("resume took %d ms", res.ResumeMs)
	}
}

func TestResumeAfterManifestChangeFails(t *testing.T) {
	work := buildWorkspace(t, false)
	if _, err := run(t, work, false); err != nil {
		t.Fatal(err)
	}
	// Append a harmless comment; the input digest changes.
	mfPath := filepath.Join(work, "material.txt")
	body, _ := os.ReadFile(mfPath)
	if err := os.WriteFile(mfPath, append(body, []byte("# changed\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := run(t, work, true)
	if model.CodeOf(err) != model.CodeCheckpointMismatch {
		t.Fatalf("code = %q (%v)", model.CodeOf(err), err)
	}
	if res == nil || res.Status != Failed {
		t.Fatalf("res = %+v", res)
	}
}

func TestCorruptCheckpointFails(t *testing.T) {
	work := buildWorkspace(t, false)
	if _, err := run(t, work, false); err != nil {
		t.Fatal(err)
	}
	ckPath := filepath.Join(work, "state", "checkpoint.json")
	body, err := os.ReadFile(ckPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ckPath, body[:len(body)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := run(t, work, true)
	if model.CodeOf(err) != model.CodeCheckpointInvalid {
		t.Fatalf("code = %q (%v)", model.CodeOf(err), err)
	}
	if res == nil || res.Status != Failed {
		t.Fatalf("res = %+v", res)
	}
}

func TestRepeatedRunsProduceSameDigest(t *testing.T) {
	work := buildWorkspace(t, false)
	a, err := run(t, work, false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := run(t, work, false)
	if err != nil {
		t.Fatal(err)
	}
	if a.IndexDigest != b.IndexDigest {
		t.Fatalf("digest drifted: %s vs %s", a.IndexDigest, b.IndexDigest)
	}
}

func TestCachePollutionIsRejected(t *testing.T) {
	work := buildWorkspaceWith(t, false, true)
	res, err := run(t, work, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.CacheRejected < 1 {
		t.Fatalf("cache_rejected = %d", res.CacheRejected)
	}
	if res.CacheHits < 1 {
		t.Fatalf("cache_hits = %d", res.CacheHits)
	}
}

func TestMissingWorkdirIsError(t *testing.T) {
	if _, err := Run(Options{}); err == nil {
		t.Fatal("empty workdir accepted")
	}
}

// The four fixture families the specification calls for, each driven through
// the same pipeline so cross-module recovery is covered end to end.
