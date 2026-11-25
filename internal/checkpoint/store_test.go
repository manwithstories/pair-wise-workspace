package checkpoint

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"mediapipe/internal/demux"
	"mediapipe/internal/mapping"
	"mediapipe/internal/media"
	"mediapipe/internal/remux"
	"mediapipe/internal/testkit"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	h.Write([]byte("prefix"))
	state, _ := h.(interface{ MarshalBinary() ([]byte, error) }).MarshalBinary()
	prog := remux.Progress{
		Bytes: 4096, Packets: 12, Clusters: 2, HashState: state,
		TimeStart: 0, TimeEnd: 2 * media.Second, HaveTime: true,
	}
	if err := store.Save("job-1", "out.mkv", "matroska", prog); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load("job-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Bytes != 4096 || loaded.Packets != 12 || loaded.TimeEnd != 2*media.Second {
		t.Fatalf("loaded=%+v", loaded)
	}
	resume := loaded.Resume()
	if resume.Bytes != prog.Bytes || !bytes.Equal(resume.HashState, prog.HashState) {
		t.Fatalf("resume=%+v", resume)
	}
}

func TestLoadMissingReturnsNil(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	st, err := store.Load("absent")
	if err != nil || st != nil {
		t.Fatalf("st=%+v err=%v", st, err)
	}
}

func TestChecksumDetectsTampering(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir)
	store.Save("job", "out.mkv", "matroska", remux.Progress{Bytes: 10})
	path := filepath.Join(dir, "job.ckpt.json")
	data, _ := os.ReadFile(path)
	data = bytes.Replace(data, []byte(`"bytes": 10`), []byte(`"bytes": 20`), 1)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("job"); err == nil {
		t.Fatal("tampered checkpoint should fail checksum")
	}
}

func TestDeleteRemovesCheckpoint(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	store.Save("job", "out.mkv", "matroska", remux.Progress{Bytes: 10})
	if err := store.Delete("job"); err != nil {
		t.Fatal(err)
	}
	if st, _ := store.Load("job"); st != nil {
		t.Fatal("checkpoint still present")
	}
	if err := store.Delete("job"); err != nil {
		t.Fatalf("idempotent delete failed: %v", err)
	}
}

// TestResumeAcrossProcessSimulation writes half the stream, checkpoints,
// truncates the output, then resumes and compares the final digest with a
// complete run.
func TestResumeAcrossProcessSimulation(t *testing.T) {
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
	dir := t.TempDir()
	store, _ := NewStore(filepath.Join(dir, "ckpt"))
	outPath := filepath.Join(dir, "out.mkv")
	opts := remux.Options{Target: mapping.TargetMatroska, ClusterSpan: 2 * media.Second, ClusterCRC: true}

	// Uninterrupted reference.
	var ref bytes.Buffer
	sink, _ := remux.NewSink(&ref, plan, in.Tracks, opts)
	for _, p := range packets {
		if err := sink.WritePacket(p); err != nil {
			t.Fatal(err)
		}
	}
	refSum, err := sink.Close()
	if err != nil {
		t.Fatal(err)
	}

	// First process: write part, flush checkpoint, persist bytes.
	var part bytes.Buffer
	s1, _ := remux.NewSink(&part, plan, in.Tracks, opts)
	for _, p := range packets[:len(packets)/2] {
		if err := s1.WritePacket(p); err != nil {
			t.Fatal(err)
		}
	}
	prog, err := s1.Flush()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save("resume-job", outPath, "matroska", prog); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outPath, part.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	// Second process: load, truncate, resume.
	st, err := store.Load("resume-job")
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		t.Fatal("checkpoint missing")
	}
	if err := st.TruncateOutput(outPath); err != nil {
		t.Fatal(err)
	}
	existing, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	resumeOpts := opts
	resumeOpts.Resume = st.Resume()
	var out bytes.Buffer
	out.Write(existing)
	s2, err := remux.NewSink(&out, plan, in.Tracks, resumeOpts)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range packets[st.Packets:] {
		if err := s2.WritePacket(p); err != nil {
			t.Fatal(err)
		}
	}
	resSum, err := s2.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resSum.Digest != refSum.Digest {
		t.Fatalf("resumed digest %x != full %x", resSum.Digest[:8], refSum.Digest[:8])
	}
	if !bytes.Equal(out.Bytes(), ref.Bytes()) {
		t.Fatal("resumed bytes differ from reference")
	}
	if err := store.Delete("resume-job"); err != nil {
		t.Fatal(err)
	}
}

func TestTruncateRejectsShortOutput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "short.bin")
	os.WriteFile(path, []byte("abc"), 0o644)
	st := &State{Bytes: 100}
	if err := st.TruncateOutput(path); err == nil {
		t.Fatal("expected error for short output")
	}
}
