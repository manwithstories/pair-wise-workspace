package remux

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"avpipe/internal/demux"
	"avpipe/internal/fixture"
	"avpipe/internal/media"
)

// readAll demuxes a fixture into packets with per-track arrival order.
func readAll(t *testing.T, path string) []media.Packet {
	t.Helper()
	in, err := demux.Open(path, demux.Options{})
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer in.Close()
	var out []media.Packet
	for {
		p, err := in.Packet()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("packet: %v", err)
		}
		out = append(out, p)
	}
	return out
}

func generateFixture(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	set, err := fixture.GenerateAll(dir, 17)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range set.Files {
		if filepath.Base(f.Path) == name {
			return f.Path
		}
	}
	t.Fatalf("fixture %q missing", name)
	return ""
}

func TestRemuxMP4Lossless(t *testing.T) {
	src := generateFixture(t, "good.mp4")
	out := filepath.Join(t.TempDir(), "copy.mp4")

	in, err := demux.Open(src, demux.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()

	ctx := context.Background()
	cfg := Config{Format: "mp4", Path: out}
	for _, tr := range in.Tracks {
		cfg.Tracks = append(cfg.Tracks, TrackSpec{Track: tr})
	}
	w, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	for _, tr := range in.Tracks {
		cp, ok := in.CodecParameters(tr.ID)
		if !ok {
			t.Fatalf("no codec params for %s", tr.ID)
		}
		if err := w.AcceptParameters(tr.ID, cp); err != nil {
			t.Fatalf("accept params %s: %v", tr.ID, err)
		}
	}
	if err := w.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	for {
		p, err := in.Packet()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := w.WritePacket(p); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := w.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Re-open the output and assert every payload byte survived untouched.
	srcPkts := readAll(t, src)
	dstPkts := readAll(t, out)
	if len(srcPkts) != len(dstPkts) {
		t.Fatalf("packet count %d -> %d", len(srcPkts), len(dstPkts))
	}
	srcHashes := map[string]string{}
	for _, p := range srcPkts {
		srcHashes[p.TrackID+"#"+itoa(p.Seq)] = p.PayloadHash()
	}
	for _, p := range dstPkts {
		key := p.TrackID + "#" + itoa(p.Seq)
		want, ok := srcHashes[key]
		if !ok {
			// Sequence numbering restarts at 0 on the copy; match by payload
			// multiset instead.
			continue
		}
		if p.PayloadHash() != want {
			t.Fatalf("%s payload changed", key)
		}
	}
	// Stronger invariant: the multiset of payload hashes is identical.
	if !sameMultiset(srcPkts, dstPkts) {
		t.Fatal("output payload multiset differs from source")
	}
}

func TestRemuxRejectsUnsupportedCodec(t *testing.T) {
	// An AAC track cannot be copied into MXF without rewriting samples.
	out := filepath.Join(t.TempDir(), "bad.mxf")
	tr := media.Track{ID: "a0", Kind: media.TrackAudio, Codec: "aac",
		TimeBase: media.MustRat(1, 48000)}
	_, err := Open(context.Background(), Config{
		Format: "mxf", Path: out, Tracks: []TrackSpec{{Track: tr}},
	})
	if err == nil {
		t.Fatal("expected rejection")
	}
	if media.KindOf(err) != media.KindCorrupt {
		t.Fatalf("kind = %v (%v)", media.KindOf(err), err)
	}
}

func TestRemuxRejectsInvalidTrack(t *testing.T) {
	out := filepath.Join(t.TempDir(), "bad.mp4")
	tr := media.Track{ID: "v0", Kind: media.TrackVideo, Codec: "h264",
		TimeBase: media.MustRat(1, 90000)}
	tr.Display.Invalid = true
	tr.Display.InvalidReason = "crop exceeds frame"
	_, err := Open(context.Background(), Config{
		Format: "mp4", Path: out, Tracks: []TrackSpec{{Track: tr}},
	})
	if media.KindOf(err) != media.KindPolicy {
		t.Fatalf("kind = %v", media.KindOf(err))
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatal("invalid track must not publish output")
	}
}

func TestRemuxCancelLeavesNoFile(t *testing.T) {
	src := generateFixture(t, "good.mp4")
	dir := t.TempDir()
	out := filepath.Join(dir, "cancel.mp4")
	in, err := demux.Open(src, demux.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cfg := Config{Format: "mp4", Path: out}
	for _, tr := range in.Tracks {
		cfg.Tracks = append(cfg.Tracks, TrackSpec{Track: tr})
	}
	w, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range in.Tracks {
		cp, _ := in.CodecParameters(tr.ID)
		w.AcceptParameters(tr.ID, cp) //nolint:errcheck
	}
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	// Write a few packets then cancel.
	for i := 0; i < 3; i++ {
		p, err := in.Packet()
		if err != nil {
			break
		}
		w.WritePacket(p) //nolint:errcheck
	}
	cancel()
	if err := w.Commit(ctx); err == nil {
		t.Fatal("commit after cancel should fail")
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatal("canceled run left a published file")
	}
	// No stray temp files either.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		t.Fatalf("leftover file %q", e.Name())
	}
}

func sameMultiset(a, b []media.Packet) bool {
	if len(a) != len(b) {
		return false
	}
	counts := map[string]int{}
	for _, p := range a {
		counts[p.PayloadHash()]++
	}
	for _, p := range b {
		counts[p.PayloadHash()]--
		if counts[p.PayloadHash()] < 0 {
			return false
		}
	}
	for _, v := range counts {
		if v != 0 {
			return false
		}
	}
	return true
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// TestRemuxMXFToMXFLossless verifies an MXF can be copied to MXF, which
// requires the writer to declare the video frame rate the muxer demands, and
// that the encoded payloads survive.
func TestRemuxMXFToMXFLossless(t *testing.T) {
	src := generateFixture(t, "good.mxf")
	out := filepath.Join(t.TempDir(), "copy.mxf")
	in, err := demux.Open(src, demux.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	ctx := context.Background()
	cfg := Config{Format: "mxf", Path: out}
	for _, tr := range in.Tracks {
		cfg.Tracks = append(cfg.Tracks, TrackSpec{Track: tr})
	}
	w, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer w.Abort()
	for _, tr := range in.Tracks {
		cp, _ := in.CodecParameters(tr.ID)
		if err := w.AcceptParameters(tr.ID, cp); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	for {
		p, err := in.Packet()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := w.WritePacket(p); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := w.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if !sameMultiset(readAll(t, src), readAll(t, out)) {
		t.Fatal("MXF copy payload multiset differs from source")
	}
}
