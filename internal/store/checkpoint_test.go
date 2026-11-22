package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"mediacore/internal/media"
)

func sampleCheckpoint() *Checkpoint {
	return &Checkpoint{
		InputPath:        "in.mp4",
		InputSHA256:      "aa",
		ConfigDigest:     "bb",
		ContainerVersion: "mp4/1.0",
		CodecVersion:     "pcm/1.0",
		OutputRate:       48000,
		OutputChannels:   2,
		LoudnessDBFS:     -1,
		BlockFrames:      4800,
		TotalBlocks:      10,
	}
}

func bindingOf(cp *Checkpoint) Binding {
	return Binding{
		InputSHA256:      cp.InputSHA256,
		ConfigDigest:     cp.ConfigDigest,
		ContainerVersion: cp.ContainerVersion,
		CodecVersion:     cp.CodecVersion,
		OutputRate:       cp.OutputRate,
		OutputChannels:   cp.OutputChannels,
		LoudnessDBFS:     cp.LoudnessDBFS,
		BlockFrames:      cp.BlockFrames,
	}
}

// TestCheckpointRoundTrip checks save/load preserves the binding.
func TestCheckpointRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cp := sampleCheckpoint()
	if err := Save(dir, cp); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.InputSHA256 != cp.InputSHA256 || got.BlockFrames != cp.BlockFrames {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if err := Check(got, bindingOf(cp)); err != nil {
		t.Fatalf("matching binding rejected: %v", err)
	}
}

// TestCheckpointMissingIsNotAnError lets a fresh work directory start clean.
func TestCheckpointMissingIsNotAnError(t *testing.T) {
	cp, err := Load(t.TempDir())
	if err != nil || cp != nil {
		t.Fatalf("got %v, %v", cp, err)
	}
}

// TestCheckpointConflictListsAllFields reports every mismatch at once.
func TestCheckpointConflictListsAllFields(t *testing.T) {
	cp := sampleCheckpoint()
	b := bindingOf(cp)
	b.OutputChannels = 1
	b.LoudnessDBFS = -3
	err := Check(cp, b)
	if err == nil {
		t.Fatal("expected conflict")
	}
	me, ok := media.AsError(err)
	if !ok || me.Code != media.CodeCheckpointConflict {
		t.Fatalf("got %v", err)
	}
	if me.ExitCode() != media.ExitCheckpoint {
		t.Fatalf("exit %d", me.ExitCode())
	}
	for _, field := range []string{"output_channels", "loudness_dbfs"} {
		if !contains(me.Message, field) {
			t.Errorf("message lacks %s: %s", field, me.Message)
		}
	}
}

// TestCorruptCheckpointRejected refuses a torn or truncated file.
func TestCorruptCheckpointRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected corrupt checkpoint")
	}
	if me, _ := media.AsError(err); me == nil || me.Code != media.CodeCheckpointCorrupt {
		t.Fatalf("got %v", err)
	}
}

// TestBlockDigestVerification proves resume trusts only intact blocks.
func TestBlockDigestVerification(t *testing.T) {
	dir := t.TempDir()
	var recs []BlockRecord
	for i, body := range [][]byte{[]byte("one"), []byte("two"), []byte("three")} {
		rec, err := WriteBlockAtomically(dir, i, body)
		if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, rec)
	}
	sha, err := VerifyBlocks(dir, recs)
	if err != nil {
		t.Fatalf("intact blocks rejected: %v", err)
	}
	if len(sha) != 64 {
		t.Fatalf("digest %q", sha)
	}
	if err := os.WriteFile(BlockPath(dir, 1), []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBlocks(dir, recs); err == nil {
		t.Fatal("tampered block accepted")
	}
}

// TestAssembleAndVerify checks concatenation order and digest binding.
func TestAssembleAndVerify(t *testing.T) {
	dir := t.TempDir()
	parts := [][]byte{[]byte("alpha"), []byte("beta"), []byte("gamma")}
	var recs []BlockRecord
	for i, p := range parts {
		rec, err := WriteBlockAtomically(dir, i, p)
		if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, rec)
	}
	sha, err := VerifyBlocks(dir, recs)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Assemble(dir, recs, sha)
	if err != nil {
		t.Fatal(err)
	}
	if got != sha {
		t.Fatalf("assemble digest %s != %s", got, sha)
	}
	data, err := os.ReadFile(OutputPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "alphabetagamma" {
		t.Fatalf("output = %q", data)
	}
	if _, err := Assemble(dir, recs, "00"); err == nil {
		t.Fatal("digest mismatch accepted")
	}
}

// TestAtomicWriteLeavesNoTempFile confirms a successful write does not leak a
// .part file into the work directory.
func TestAtomicWriteLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.json")
	if err := WriteFileAtomic(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "x.json" {
		t.Fatalf("unexpected entries: %v", entries)
	}
}

// TestProvenanceAppendRead round-trips records and tolerates a torn last line.
func TestProvenanceAppendRead(t *testing.T) {
	dir := t.TempDir()
	p := &Provenance{
		Mode:      "demo",
		InputPath: "in.mp4",
		Params:    Params{Rate: 48000, Channels: 2, LoudnessDBFS: -1, BlockFrames: 4800},
		Output:    OutputSummary{SHA256: "deadbeef", SamplesOut: 100},
		Errors:    []ErrorRecord{{Code: "TRUNCATED_MDAT", Offset: 5, HasOff: true, Track: 1}},
	}
	if err := Append(dir, p); err != nil {
		t.Fatal(err)
	}
	if err := Append(dir, p); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash mid-write.
	f, err := os.OpenFile(ProvenancePath(dir), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"version":1,"mode":"demo"`)
	f.Close()

	recs, err := ReadAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("records = %d, want 2 (torn line skipped)", len(recs))
	}
	if recs[0].Output.SHA256 != "deadbeef" || recs[0].Errors[0].Offset != 5 {
		t.Fatalf("record lost detail: %+v", recs[0])
	}
}

// TestProvenanceIsJSONLines checks each line decodes independently.
func TestProvenanceIsJSONLines(t *testing.T) {
	dir := t.TempDir()
	if err := Append(dir, &Provenance{Mode: "demo"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(ProvenancePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	line := data[:len(data)-1]
	var p Provenance
	if err := json.Unmarshal(line, &p); err != nil {
		t.Fatalf("line is not standalone JSON: %v", err)
	}
	if p.Version != ProvenanceVersion {
		t.Fatalf("version %d", p.Version)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
