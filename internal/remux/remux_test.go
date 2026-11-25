package remux

import (
	"crypto/sha256"
	"path/filepath"
	"testing"

	"mediaarchive/internal/clock"
	"mediaarchive/internal/demux"
	"mediaarchive/internal/fixture"
	"mediaarchive/internal/model"
)

// norm builds a normalized sample carrying a deterministic payload and hash.
func norm(track, seq, chunk uint32, dts uint64, payload []byte) clock.NormalizedSample {
	h := sha256.Sum256(payload)
	return clock.NormalizedSample{
		Sample: demux.Sample{
			TrackID: track, Seq: seq, ChunkSeq: chunk, DTS: dts,
			Duration: 1000, Size: uint32(len(payload)), Payload: payload, SHA256: h,
		},
		DTSNorm: model.NewRational(int64(dts), 90000),
		DurNorm: model.NewRational(1000, 90000),
	}
}

func TestLosslessPacketHashes(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.mp4")
	wr, err := NewWriter(out, []string{"isom"})
	if err != nil {
		t.Fatal(err)
	}
	payloads := [][]byte{
		[]byte("packet-one-payload"),
		[]byte("packet-two-payload!!!"),
		[]byte("packet-three-payload?"),
	}
	var in []clock.NormalizedSample
	for i, p := range payloads {
		in = append(in, norm(1, uint32(i+1), 0, uint64(i*1000), p))
	}
	for _, s := range in {
		if err := wr.AddSample(s); err != nil {
			wr.Abort()
			t.Fatal(err)
		}
	}
	tracks := []demux.TrackMeta{{ID: 1, Kind: demux.KindVideo, Timescale: 90000}}
	if _, err := wr.Close(tracks); err != nil {
		t.Fatal(err)
	}
	// Re-open the output through the real demuxer and compare every packet.
	c, err := demux.Open(out)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer c.Close()
	idx := 0
	err = c.StreamSamples(demux.StreamOptions{}, func(s demux.Sample) error {
		got := sha256.Sum256(s.Payload)
		want := sha256.Sum256(payloads[idx])
		if got != want {
			t.Fatalf("packet %d re-encoded: hash mismatch", idx)
		}
		idx++
		return nil
	})
	if err != nil {
		t.Fatalf("stream output: %v", err)
	}
	if idx != len(payloads) {
		t.Fatalf("packets out = %d, want %d", idx, len(payloads))
	}
}

func TestTruncatedSampleDropped(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "trunc.mp4")
	wr, _ := NewWriter(out, nil)
	good := norm(1, 1, 0, 0, []byte("good"))
	bad := norm(1, 2, 0, 1000, []byte("partial"))
	bad.Flags |= demux.FlagTruncated
	if err := wr.AddSample(good); err != nil {
		t.Fatal(err)
	}
	if err := wr.AddSample(bad); err != nil {
		t.Fatal(err)
	}
	stats, err := wr.Close([]demux.TrackMeta{{ID: 1, Kind: demux.KindVideo, Timescale: 90000}})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Dropped != 1 || stats.PacketsKept != 1 {
		t.Fatalf("dropped=%d kept=%d, want 1/1", stats.Dropped, stats.PacketsKept)
	}
}

func TestAtomicOutputLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "atomic.mp4")
	wr, _ := NewWriter(out, nil)
	if err := wr.AddSample(norm(1, 1, 0, 0, []byte("x"))); err != nil {
		t.Fatal(err)
	}
	if _, err := wr.Close([]demux.TrackMeta{{ID: 1, Kind: demux.KindVideo, Timescale: 90000}}); err != nil {
		t.Fatal(err)
	}
	entries, err := osReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.HasPrefix(e, ".remux-") {
			t.Fatalf("temp file not cleaned: %s", e)
		}
	}
}

func TestDeterministicAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	build := func(path string) {
		wr, _ := NewWriter(path, []string{"isom"})
		for i := 0; i < 20; i++ {
			p := make([]byte, 32)
			for j := range p {
				p[j] = byte(i*7 + j)
			}
			if err := wr.AddSample(norm(1, uint32(i+1), uint32(i/6), uint64(i*1000), p)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := wr.Close([]demux.TrackMeta{{ID: 1, Kind: demux.KindVideo, Timescale: 90000}}); err != nil {
			t.Fatal(err)
		}
	}
	p1, p2 := filepath.Join(dir, "a.mp4"), filepath.Join(dir, "b.mp4")
	build(p1)
	build(p2)
	h1 := sha256.Sum256(mustRead(t, p1))
	h2 := sha256.Sum256(mustRead(t, p2))
	if h1 != h2 {
		t.Fatal("repeated remux produced different output bytes")
	}
}

func TestRoundTripFixture(t *testing.T) {
	dir := t.TempDir()
	if _, err := fixture.GenerateAll(dir, []string{fixture.CaseNormal}, 17); err != nil {
		t.Fatal(err)
	}
	src, err := demux.Open(filepath.Join(dir, fixture.CaseNormal+".mov"))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	wr, _ := NewWriter(filepath.Join(dir, "rt.mp4"), src.Brands)
	count := 0
	err = src.StreamSamples(demux.StreamOptions{}, func(s demux.Sample) error {
		s.SHA256 = sha256.Sum256(s.Payload)
		ns := clock.NormalizedSample{Sample: s}
		if err := wr.AddSample(ns); err != nil {
			return err
		}
		count++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wr.Close(src.Tracks); err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatal("fixture round trip wrote no packets")
	}
}
