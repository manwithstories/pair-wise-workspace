package checkpoint

import (
	"os"
	"path/filepath"
	"testing"

	"mediaroute/internal/model"
)

func TestSaveLoadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.chk")
	in := &Checkpoint{
		InputDigest: "abc123",
		Cursors:     map[string]uint64{"v1": 42, "a1": 7},
		Committed:   []SegmentRecord{{Index: 0, SHA256: "aa"}, {Index: 1, SHA256: "bb"}},
	}
	if err := Save(path, in); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file must be renamed away")
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.InputDigest != "abc123" || got.Cursors["v1"] != 42 || len(got.Committed) != 2 {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if err := got.VerifyDigest("abc123"); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMissing(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.chk"))
	if err != nil || c != nil {
		t.Fatalf("missing checkpoint must be (nil, nil), got %v %v", c, err)
	}
}

func TestDigestMismatch(t *testing.T) {
	c := &Checkpoint{InputDigest: "old"}
	err := c.VerifyDigest("new")
	me, ok := err.(*model.MediaError)
	if !ok || me.Code != model.ErrCheckpoint {
		t.Fatalf("want ErrCheckpoint, got %v", err)
	}
	if model.ExitCode(me) != model.ExitCheckpoint {
		t.Fatalf("exit code %d", model.ExitCode(me))
	}
}

func TestCommittedIndex(t *testing.T) {
	c := &Checkpoint{Committed: []SegmentRecord{{Index: 3, SHA256: "x"}}}
	if rec, ok := c.CommittedIndex(3); !ok || rec.SHA256 != "x" {
		t.Fatal("committed segment not found")
	}
	if _, ok := c.CommittedIndex(4); ok {
		t.Fatal("uncommitted segment must not be found")
	}
}
