package remux

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"mediapipe/internal/mapping"
	"mediapipe/internal/media"
	"mediapipe/internal/testkit"
)

// resumeFixture writes the first half of packets, flushes to a checkpoint,
// then resumes into a fresh buffer that already holds the flushed bytes.
func resumeFixture(t *testing.T, target mapping.Target, tracksIn []media.Track) (full, resumed Summary, equal bool) {
	t.Helper()
	clean, _, _, _ := testkit.Generate(73)
	in := demuxFixture(t, clean.Bytes)
	tracks := tracksIn
	if tracks == nil {
		tracks = in.Tracks
	}
	plan, err := mapping.NewResolver(target, mapping.DefaultRules()).Resolve(tracks)
	if err != nil {
		t.Fatal(err)
	}
	packets := remapPackets(plan, in.Packets)
	cut := len(packets) / 2
	opts := Options{Target: target, ClusterSpan: 2 * media.Second, ClusterCRC: true}

	// Uninterrupted run.
	var whole bytes.Buffer
	sink, err := NewSink(&whole, plan, tracks, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range packets {
		if err := sink.WritePacket(p); err != nil {
			t.Fatal(err)
		}
	}
	full, err = sink.Close()
	if err != nil {
		t.Fatal(err)
	}

	// Interrupted run: stop at a cluster boundary, checkpoint, resume.
	var part bytes.Buffer
	s1, err := NewSink(&part, plan, tracks, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range packets[:cut] {
		if err := s1.WritePacket(p); err != nil {
			t.Fatal(err)
		}
	}
	progress, err := s1.Flush()
	if err != nil {
		t.Fatal(err)
	}
	if progress.Bytes > int64(part.Len()) {
		t.Fatalf("progress bytes=%d buffer=%d", progress.Bytes, part.Len())
	}
	// The checkpoint trails the written bytes: the open cluster/fragment is
	// discarded and resumed from the source.
	part.Truncate(int(progress.Bytes))
	resumeOpts := opts
	resumeOpts.Resume = &ResumeState{Progress: progress, TruncateTo: progress.Bytes}
	s2, err := NewSink(&part, plan, tracks, resumeOpts)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range packets[progress.Packets:] {
		if err := s2.WritePacket(p); err != nil {
			t.Fatal(err)
		}
	}
	resumed, err = s2.Close()
	if err != nil {
		t.Fatal(err)
	}
	return full, resumed, bytes.Equal(whole.Bytes(), part.Bytes())
}

func TestMKVResumeMatchesFullRun(t *testing.T) {
	full, resumed, equal := resumeFixture(t, mapping.TargetMatroska, nil)
	if full.Digest != resumed.Digest {
		t.Fatalf("digest mismatch: full=%x resumed=%x", full.Digest[:8], resumed.Digest[:8])
	}
	if !equal {
		t.Fatal("resumed bytes differ from uninterrupted run")
	}
	if full.Packets != resumed.Packets {
		t.Fatalf("packet count differs: %d vs %d", full.Packets, resumed.Packets)
	}
	if full.Bytes != resumed.Bytes {
		t.Fatalf("byte count differs: %d vs %d", full.Bytes, resumed.Bytes)
	}
}

func TestFMP4ResumeMatchesFullRun(t *testing.T) {
	tracks := []media.Track{}
	clean, _, _, _ := testkit.Generate(73)
	in := demuxFixture(t, clean.Bytes)
	tracks = append(tracks, in.Tracks[:2]...)
	full, resumed, equal := resumeFixture(t, mapping.TargetFMP4, tracks)
	if full.Digest != resumed.Digest || !equal {
		t.Fatalf("fMP4 resume mismatch: full=%x resumed=%x equal=%v", full.Digest[:8], resumed.Digest[:8], equal)
	}
}

func TestResumeRejectsEmptyHashState(t *testing.T) {
	clean, _, _, _ := testkit.Generate(73)
	in := demuxFixture(t, clean.Bytes)
	plan, _ := mapping.NewResolver(mapping.TargetMatroska, mapping.DefaultRules()).Resolve(in.Tracks)
	var buf bytes.Buffer
	_, err := NewSink(&buf, plan, in.Tracks, Options{
		Target: mapping.TargetMatroska,
		Resume: &ResumeState{},
	})
	if err == nil || media.ClassOf(err) != media.ClassInterrupted {
		t.Fatalf("err=%v class=%v", err, media.ClassOf(err))
	}
}

func TestFlushProducesUsableHashState(t *testing.T) {
	clean, _, _, _ := testkit.Generate(73)
	in := demuxFixture(t, clean.Bytes)
	plan, _ := mapping.NewResolver(mapping.TargetMatroska, mapping.DefaultRules()).Resolve(in.Tracks)
	packets := remapPackets(plan, in.Packets)
	var buf bytes.Buffer
	sink, _ := NewSink(&buf, plan, in.Tracks, Options{Target: mapping.TargetMatroska, ClusterSpan: 2 * media.Second})
	for _, p := range packets[:len(packets)/3] {
		sink.WritePacket(p)
	}
	progress, err := sink.Flush()
	if err != nil {
		t.Fatal(err)
	}
	if len(progress.HashState) == 0 {
		t.Fatal("empty hash state")
	}
	// A SHA-256 digest restored from state must equal hashing the flushed
	// bytes; bytes after the checkpoint belong to the open cluster.
	h := sha256.Sum256(buf.Bytes()[:progress.Bytes])
	restored := sha256.New()
	if err := restored.(interface{ UnmarshalBinary([]byte) error }).UnmarshalBinary(progress.HashState); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored.Sum(nil), h[:]) {
		t.Fatal("restored digest does not match hashed bytes")
	}
}
