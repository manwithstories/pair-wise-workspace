package audit

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"mediapipe/internal/demux"
	"mediapipe/internal/mapping"
	"mediapipe/internal/media"
	"mediapipe/internal/remux"
	"mediapipe/internal/testkit"
	"mediapipe/internal/timeline"
)

func fixture(t *testing.T) (*demux.Result, *mapping.Plan, []media.Packet) {
	t.Helper()
	clean, _, _, _ := testkit.Generate(73)
	in, err := demux.Demux(bytes.NewReader(clean.Bytes), demux.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := mapping.NewResolver(mapping.TargetMatroska, mapping.DefaultRules()).Resolve(in.Tracks)
	if err != nil {
		t.Fatal(err)
	}
	var packets []media.Packet
	for _, p := range in.Packets {
		if _, ok := plan.Remap(p.Track); ok {
			packets = append(packets, p)
		}
	}
	return in, plan, packets
}

func TestManifestRecordsTracksAndTimeline(t *testing.T) {
	in, plan, packets := fixture(t)
	var buf bytes.Buffer
	sink, _ := remux.NewSink(&buf, plan, in.Tracks, remux.Options{
		Target: mapping.TargetMatroska, ClusterSpan: 2 * media.Second, ClusterCRC: true,
	})
	for _, p := range packets {
		if err := sink.WritePacket(p); err != nil {
			t.Fatal(err)
		}
	}
	sum, err := sink.Close()
	if err != nil {
		t.Fatal(err)
	}
	engine, err := timeline.New(timeline.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range packets {
		ev := timeline.Event{
			Key: string(rune('a' + p.Track)) + "-" + itoa(int64(p.DTS)),
			Track: p.Track, Time: p.DTS, Duration: p.Duration, Payload: p.Data,
			PTS: p.PTS,
		}
		if _, _, err := engine.Push(ev); err != nil {
			t.Fatalf("watermark rejected in-order fixture packet: %v", err)
		}
	}
	b := NewBuilder("job-1", "in.mkv", mapping.TargetMatroska)
	b.SetRate(5_000_000, 5_010_000)
	b.SetOutput(sum)
	b.SetTimeline(engine, engine.Summaries(), engine.ReplayDigest())
	b.SetBufferPeak(1 << 20)
	b.SetVerification(0, 0)
	b.SetSkew(0)
	b.SetTracks(plan, packets)
	if err := b.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	m := b.Manifest()
	if !m.Committed {
		t.Fatal("manifest not committed")
	}
	if len(m.Tracks) != 3 {
		t.Fatalf("tracks=%d", len(m.Tracks))
	}
	if m.OutputDigest == "" {
		t.Fatal("missing output digest")
	}
	if m.ReplayDigest == "" {
		t.Fatal("missing replay digest")
	}
	if m.DeltaPct <= 0 || m.DeltaPct > 0.5 {
		t.Fatalf("delta=%.4f", m.DeltaPct)
	}
	var total int
	for _, tr := range m.Tracks {
		total += tr.Packets
	}
	if total != len(packets) {
		t.Fatalf("track packet sum=%d want %d", total, len(packets))
	}
}

func TestCommitRejectsHashMismatch(t *testing.T) {
	b := NewBuilder("job", "in.mkv", mapping.TargetMatroska)
	b.SetVerification(1, 0)
	b.SetSkew(0)
	err := b.Commit()
	if err == nil || media.ClassOf(err) != media.ClassFatal {
		t.Fatalf("err=%v", err)
	}
	if b.Manifest().Committed {
		t.Fatal("manifest must not commit")
	}
	if b.Manifest().Reason == "" {
		t.Fatal("missing reason")
	}
}

func TestCommitRejectsCRCEdits(t *testing.T) {
	b := NewBuilder("job", "in.mkv", mapping.TargetMatroska)
	b.SetVerification(0, 2)
	if err := b.Commit(); err == nil {
		t.Fatal("expected crc failure")
	}
}

func TestCommitRejectsSkewBeyondOneMillisecond(t *testing.T) {
	b := NewBuilder("job", "in.mkv", mapping.TargetMatroska)
	b.SetSkew(media.Millisecond + 1)
	if err := b.Commit(); err == nil {
		t.Fatal("expected skew failure")
	}
	b2 := NewBuilder("job", "in.mkv", mapping.TargetMatroska)
	b2.SetSkew(media.Millisecond)
	if err := b2.Commit(); err != nil {
		t.Fatalf("boundary skew should pass: %v", err)
	}
}

func TestCommitRejectsQuarantine(t *testing.T) {
	engine, _ := timeline.New(timeline.DefaultConfig())
	engine.Push(timeline.Event{Key: "a", Track: 1, Time: 10 * media.Second})
	engine.Push(timeline.Event{Key: "b", Track: 1, Time: 1 * media.Second})
	b := NewBuilder("job", "in.mkv", mapping.TargetMatroska)
	b.SetTimeline(engine, engine.Summaries(), engine.ReplayDigest())
	if err := b.Commit(); err == nil {
		t.Fatal("expected quarantine failure")
	}
}

func TestManifestWriteReadAndDigest(t *testing.T) {
	dir := t.TempDir()
	b := NewBuilder("job-1", "in.mkv", mapping.TargetMatroska)
	b.SetVerification(0, 0)
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "manifest.json")
	if err := b.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	m, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.JobID != "job-1" || m.Target != "matroska" {
		t.Fatalf("round trip lost fields: %+v", m)
	}
	d1, err := b.Digest()
	if err != nil || len(d1) != 32 {
		t.Fatalf("digest err=%v", err)
	}
	b2 := NewBuilder("job-2", "in.mkv", mapping.TargetMatroska)
	b2.SetVerification(0, 0)
	d2, _ := b2.Digest()
	if d1 == d2 {
		t.Fatal("different jobs produced identical digests")
	}
}

func TestManifestJSONSchema(t *testing.T) {
	b := NewBuilder("job", "in.webm", mapping.TargetWebM)
	b.SetVerification(0, 0)
	data, err := json.Marshal(b.Manifest())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"target_bps", "actual_bps", "delta_pct", "payload_hash_mismatch",
		"crc_errors", "manifest_skew_ns", "buffer_peak_bytes", "replay_digest"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("manifest missing %q", key)
		}
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func TestPackageHashStable(t *testing.T) {
	b := NewBuilder("job", "in.mkv", mapping.TargetMatroska)
	b.SetVerification(0, 0)
	b.Commit()
	m := b.Manifest()
	h1 := PackageHash([]byte("output"), &m)
	h2 := PackageHash([]byte("output"), &m)
	if h1 != h2 || h1 == "" {
		t.Fatalf("package hash unstable: %q %q", h1, h2)
	}
	h3 := PackageHash([]byte("output2"), &m)
	if h3 == h1 {
		t.Fatal("different output should change package hash")
	}
}

func TestBuilderSetOutputDerivesDuration(t *testing.T) {
	b := NewBuilder("job", "in.mkv", mapping.TargetMatroska)
	b.SetOutput(remux.Summary{TimeStart: 100, TimeEnd: 1100})
	m := b.Manifest()
	if m.Duration != 1000 {
		t.Fatalf("duration=%d", m.Duration)
	}
}

func TestWriteFileCreatesDirectories(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deeper", "manifest.json")
	b := NewBuilder("job", "in.mkv", mapping.TargetMatroska)
	if err := b.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
