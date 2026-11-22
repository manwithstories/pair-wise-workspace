package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) (string, *WAL) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wal", "archive.wal")
	w, recs, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 {
		t.Fatalf("new wal has %d records", len(recs))
	}
	return path, w
}

func TestAppendAndRecover(t *testing.T) {
	path, w := openTemp(t)
	for _, stage := range []string{"parse", "repair", "index"} {
		if _, err := w.Append(Record{Stage: stage, Op: OpBegin}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Append(Record{Stage: stage, Op: OpCommit, Digest: "d-" + stage}); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()

	w2, recs, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	if len(recs) != 6 {
		t.Fatalf("records=%d", len(recs))
	}
	if w2.Truncated != 0 {
		t.Fatalf("unexpected truncation %d", w2.Truncated)
	}
	stages := Stages(recs)
	for _, s := range []string{"parse", "repair", "index"} {
		if stages[s].Op != OpCommit || stages[s].Digest != "d-"+s {
			t.Fatalf("stage %s: %+v", s, stages[s])
		}
	}
	// New appends continue the sequence.
	r, err := w2.Append(Record{Stage: "manifest", Op: OpBegin})
	if err != nil {
		t.Fatal(err)
	}
	if r.Seq != 7 {
		t.Fatalf("seq=%d want 7", r.Seq)
	}
}

func TestTornTailDiscarded(t *testing.T) {
	path, w := openTemp(t)
	if _, err := w.Append(Record{Stage: "parse", Op: OpBegin}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append(Record{Stage: "parse", Op: OpCommit, Digest: "good"}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	// Append a half-written record: valid length, truncated JSON.
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	hdr := []byte{0x40, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	f.Write(hdr)
	f.Write([]byte(`{"seq":3,"stage":"parse"`)) // incomplete
	f.Close()

	w2, recs, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	if len(recs) != 2 {
		t.Fatalf("recovered %d records, want 2", len(recs))
	}
	if w2.Truncated == 0 {
		t.Fatal("torn tail not reported")
	}
	after, _ := os.Stat(path)
	if after.Size() >= before.Size()+int64(len(hdr))+16 {
		t.Fatalf("torn bytes not truncated: %d", after.Size())
	}
	// Recovered WAL must be usable and not replay the partial record.
	if stages := Stages(recs); stages["parse"].Op != OpCommit {
		t.Fatalf("stage=%+v", stages["parse"])
	}
}

func TestChecksumRejectsCorruption(t *testing.T) {
	path, w := openTemp(t)
	if _, err := w.Append(Record{Stage: "parse", Op: OpCommit}); err != nil {
		t.Fatal(err)
	}
	w.Close()
	// Flip a byte in the payload.
	raw, _ := os.ReadFile(path)
	raw[len(raw)-2] ^= 0xFF
	_ = os.WriteFile(path, raw, 0o644)

	w2, recs, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	if len(recs) != 0 {
		t.Fatalf("corrupt record recovered: %+v", recs)
	}
	if w2.Truncated == 0 {
		t.Fatal("corruption not truncated")
	}
}

func TestResumeFromCheckpointNoDoubleCount(t *testing.T) {
	path, w := openTemp(t)
	payload, _ := json.Marshal(map[string]int{"frames": 120})
	w.Append(Record{Stage: "parse", Op: OpBegin})
	w.Append(Record{Stage: "parse", Op: OpCommit, Payload: payload, Digest: "p"})
	w.Append(Record{Stage: "repair", Op: OpBegin})
	// Interruption after repair began but before commit.
	w.Close()

	w2, recs, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	stages := Stages(recs)
	done := make(map[string]bool)
	for name, r := range stages {
		done[name] = r.Op == OpCommit
	}
	if !done["parse"] || done["repair"] {
		t.Fatalf("resume state: %+v", done)
	}
	// A resumed run must not re-commit parse.
	var commits int
	for _, r := range recs {
		if r.Stage == "parse" && r.Op == OpCommit {
			commits++
		}
	}
	if commits != 1 {
		t.Fatalf("parse committed %d times", commits)
	}
}

func TestTruncateAtHelper(t *testing.T) {
	path, w := openTemp(t)
	w.Append(Record{Stage: "a", Op: OpCommit})
	w.Append(Record{Stage: "b", Op: OpCommit})
	w.Close()
	info, _ := os.Stat(path)
	if err := TruncateAt(path, info.Size()-3); err != nil {
		t.Fatal(err)
	}
	w2, recs, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	if len(recs) != 1 || recs[0].Stage != "a" {
		t.Fatalf("records=%+v", recs)
	}
}
