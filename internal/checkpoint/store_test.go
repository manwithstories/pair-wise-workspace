package checkpoint

import (
	"os"
	"path/filepath"
	"testing"

	"relay/internal/media"
	"relay/internal/timeline"
)

func sampleData() *Data {
	return &Data{
		InputPath: "/tmp/in.mfs", InputSize: 1234, InputSHA256: "abc",
		MapPath: "/tmp/map.json", MapSHA256: "def",
		OutputPath: "/tmp/out.mta", OutputFormat: "mta",
		InputOffset: 999, CommittedOutputLen: 4321,
		FramesConsumed: 10, FramesReleased: 8,
		Watermarks: map[uint32]int64{1: 750_000_000},
		GraceNS:    int64(media.GraceWindow),
		Quarantine: []QuarantineRec{{TrackID: 1, Sequence: 3, Reason: timeline.ReasonLateBeyondWindow}},
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "run.ckpt")
	if err := Save(p, sampleData()); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.InputOffset != 999 || got.CommittedOutputLen != 4321 ||
		got.Watermarks[1] != 750_000_000 || len(got.Quarantine) != 1 {
		t.Fatalf("round trip lost state: %+v", got)
	}
}

func TestTamperedCheckpointRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "run.ckpt")
	if err := Save(p, sampleData()); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	raw[len(raw)-5] ^= 0xFF // 翻转外层 JSON 某字节
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p)
	f := media.AsFault(err)
	if f == nil || f.Stage != media.StageCheckpoint {
		t.Fatalf("want checkpoint fault, got %v", err)
	}
}

func TestVerifyFingerprints(t *testing.T) {
	d := sampleData()
	if err := VerifyFingerprints(d, "abc", "def", 1234); err != nil {
		t.Fatalf("matching fingerprints rejected: %v", err)
	}
	if err := VerifyFingerprints(d, "xxx", "def", 1234); err == nil {
		t.Fatal("input hash change must fail")
	}
	if err := VerifyFingerprints(d, "abc", "def", 9999); err == nil {
		t.Fatal("input size change must fail")
	}
	if err := VerifyFingerprints(d, "abc", "yyy", 1234); err == nil {
		t.Fatal("map hash change must fail")
	}
}

func TestTruncateOutput(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.mta")
	if err := os.WriteFile(p, make([]byte, 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := TruncateOutput(p, 600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(p)
	if info.Size() != 600 {
		t.Fatalf("size after truncate = %d", info.Size())
	}
	if err := TruncateOutput(p, 700); err == nil {
		t.Fatal("truncating beyond committed length must fail")
	}
}

func TestDedupCodecRoundTrip(t *testing.T) {
	h := media.SumSHA256([]byte("payload"))
	in := map[uint32]map[uint64][32]byte{1: {5: h}}
	enc := EncodeDedup(in)
	dec, err := DecodeDedup(enc)
	if err != nil {
		t.Fatal(err)
	}
	if dec[1][5] != h {
		t.Fatal("hash round trip mismatch")
	}
}

func TestFromTimeline(t *testing.T) {
	st := timeline.State{GraceNS: int64(media.GraceWindow), MaxEvent: map[uint32]int64{1: int64(media.Second)}}
	wm, recs := FromTimeline(st, []*timeline.Quarantine{
		{Env: &timeline.Envelope{TrackID: 1, Frame: &media.Frame{Sequence: 2}}, Reason: "x"},
	})
	if wm[1] != int64(media.Second-media.GraceWindow) {
		t.Fatalf("watermark = %d", wm[1])
	}
	if len(recs) != 1 || recs[0].Sequence != 2 {
		t.Fatalf("quarantine conversion wrong: %+v", recs)
	}
}
