package journal

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// fakeCommitter records every packet it is asked to write so a test can assert
// that a resumed run never re-emits an already-committed packet.
type fakeCommitter struct {
	written     []writtenPacket
	checkpoints []*State
	failAfter   int // if >0, fail once this many packets were written
}

type writtenPacket struct {
	track uint32
	index int64
}

func (f *fakeCommitter) Begin(assetID string) error { return nil }

func (f *fakeCommitter) WritePacket(trackID uint32, idx int64, payload []byte) (string, error) {
	if f.failAfter > 0 && len(f.written) >= f.failAfter {
		return "", errFakeCrash
	}
	f.written = append(f.written, writtenPacket{track: trackID, index: idx})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func (f *fakeCommitter) Checkpoint(st *State) error {
	f.checkpoints = append(f.checkpoints, st)
	return nil
}

func (f *fakeCommitter) Finish() (string, int64, error) { return "out", int64(len(f.written)), nil }

var errFakeCrash = &fakeError{"simulated crash"}

type fakeError struct{ msg string }

func (e *fakeError) Error() string { return e.msg }

func makeTrack(id uint32, n int64, every int64, payloads *[]byte) TrackPlan {
	return TrackPlan{
		TrackID:         id,
		PacketCount:     n,
		CheckpointEvery: every,
		Payload: func(i int64) ([]byte, error) {
			b := []byte{byte(i), byte(id), 0xAB}
			return b, nil
		},
	}
}

func TestResumeSkipsCommittedPackets(t *testing.T) {
	dir := t.TempDir()
	j, _ := Open(dir)
	g := Guards{SourceSHA256: "sha", Adapter: "mp4", AdapterVersion: 1, ConfigDigest: "cfg"}

	// First run: 100 packets, checkpoint every 10, crash after 42.
	c1 := &fakeCommitter{failAfter: 42}
	d1 := &ResumeDriver{Journal: j, Committer: c1, AssetID: "A1", Guards: g}
	if _, _, err := d1.Run([]TrackPlan{makeTrack(1, 100, 10, nil)}); err == nil {
		t.Fatal("expected simulated crash")
	}
	// The last durable checkpoint should be at packet 39 (checkpointed every 10
	// through 39, since 42 crashed before the next multiple of 10).
	st, q, err := j.Load("A1", 1, g)
	if err != nil || q || st == nil {
		t.Fatalf("checkpoint missing: st=%v q=%v err=%v", st, q, err)
	}
	if st.CommittedThrough != 39 {
		t.Fatalf("committed through = %d want 39", st.CommittedThrough)
	}

	// Second run resumes from 40 and must not rewrite 0..39.
	c2 := &fakeCommitter{}
	d2 := &ResumeDriver{Journal: j, Committer: c2, AssetID: "A1", Guards: g}
	outcomes, dupes, err := d2.Run([]TrackPlan{makeTrack(1, 100, 10, nil)})
	if err != nil {
		t.Fatal(err)
	}
	if dupes != 0 {
		t.Fatalf("duplicates = %d", dupes)
	}
	if len(outcomes) != 1 || outcomes[0].Skipped != 40 {
		t.Fatalf("outcomes = %+v", outcomes)
	}
	if len(c2.written) != 60 {
		t.Fatalf("rewrote %d packets, want 60", len(c2.written))
	}
	if c2.written[0].index != 40 {
		t.Fatalf("first packet = %d want 40", c2.written[0].index)
	}
	seen := map[int64]bool{}
	for _, w := range c2.written {
		if seen[w.index] {
			t.Fatalf("duplicate write at %d", w.index)
		}
		seen[w.index] = true
	}
}

func TestResumeFromScratchWhenInvalidated(t *testing.T) {
	dir := t.TempDir()
	j, _ := Open(dir)
	g1 := Guards{SourceSHA256: "sha-v1", Adapter: "mp4", AdapterVersion: 1, ConfigDigest: "cfg"}
	// Prime a checkpoint.
	c1 := &fakeCommitter{}
	d1 := &ResumeDriver{Journal: j, Committer: c1, AssetID: "A1", Guards: g1}
	if _, _, err := d1.Run([]TrackPlan{makeTrack(1, 20, 5, nil)}); err != nil {
		t.Fatal(err)
	}

	// New source digest: the old checkpoint must be quarantined and the asset
	// reprocessed from packet 0.
	g2 := Guards{SourceSHA256: "sha-v2", Adapter: "mp4", AdapterVersion: 1, ConfigDigest: "cfg"}
	c2 := &fakeCommitter{}
	d2 := &ResumeDriver{Journal: j, Committer: c2, AssetID: "A1", Guards: g2}
	outcomes, _, err := d2.Run([]TrackPlan{makeTrack(1, 20, 5, nil)})
	if err != nil {
		t.Fatal(err)
	}
	if !outcomes[0].Quarantined {
		t.Fatal("expected quarantine on source change")
	}
	if outcomes[0].Skipped != 0 {
		t.Fatalf("skipped = %d want 0", outcomes[0].Skipped)
	}
	if len(c2.written) != 20 {
		t.Fatalf("wrote %d packets want 20", len(c2.written))
	}
	list, _ := j.Quarantined()
	if len(list) != 1 {
		t.Fatalf("quarantine list = %v", list)
	}
}

func TestResumeHandlesShrunkSource(t *testing.T) {
	dir := t.TempDir()
	j, _ := Open(dir)
	g := Guards{SourceSHA256: "sha", Adapter: "mp4", AdapterVersion: 1, ConfigDigest: "cfg"}
	if err := j.Save(&State{AssetID: "A1", TrackID: 1, CommittedThrough: 500, CommitCount: 501,
		SourceSHA256: "sha", Adapter: "mp4", AdapterVersion: 1, ConfigDigest: "cfg"}); err != nil {
		t.Fatal(err)
	}
	c := &fakeCommitter{}
	d := &ResumeDriver{Journal: j, Committer: c, AssetID: "A1", Guards: g}
	outcomes, _, err := d.Run([]TrackPlan{makeTrack(1, 10, 5, nil)})
	if err != nil {
		t.Fatal(err)
	}
	if outcomes[0].Skipped != 0 {
		t.Fatalf("shrunk source should restart, skipped = %d", outcomes[0].Skipped)
	}
	if len(c.written) != 10 {
		t.Fatalf("wrote %d want 10", len(c.written))
	}
}

func TestResumeStableDigestAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	j, _ := Open(dir)
	g := Guards{SourceSHA256: "sha", Adapter: "mp4", AdapterVersion: 1, ConfigDigest: "cfg"}
	track := makeTrack(1, 50, 10, nil)
	c := &fakeCommitter{}
	d := &ResumeDriver{Journal: j, Committer: c, AssetID: "A1", Guards: g}
	if _, _, err := d.Run([]TrackPlan{track}); err != nil {
		t.Fatal(err)
	}
	first := append([]writtenPacket(nil), c.written...)

	// Re-run against a fresh journal: identical write order.
	j2, _ := Open(t.TempDir())
	c2 := &fakeCommitter{}
	d2 := &ResumeDriver{Journal: j2, Committer: c2, AssetID: "A1", Guards: g}
	if _, _, err := d2.Run([]TrackPlan{track}); err != nil {
		t.Fatal(err)
	}
	if len(first) != len(c2.written) {
		t.Fatalf("length drift %d vs %d", len(first), len(c2.written))
	}
	for i := range first {
		if first[i] != c2.written[i] {
			t.Fatalf("packet %d differs: %+v vs %+v", i, first[i], c2.written[i])
		}
	}
}
