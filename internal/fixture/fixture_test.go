package fixture

import (
	"os"
	"path/filepath"
	"testing"

	"relay/internal/demux"
	"relay/internal/media"
)

const allFaults = "crc,truncated,late,duplicate,slow,interrupt"

func TestGenerateDeterministicAndReadable(t *testing.T) {
	dir1 := filepath.Join(t.TempDir(), "a")
	dir2 := filepath.Join(t.TempDir(), "b")
	o1, err := Generate(Plan{Dir: dir1, Frames: 400, Faults: splitFaults(allFaults)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(Plan{Dir: dir2, Frames: 400, Faults: splitFaults(allFaults)}); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{
		{o1.Live, filepath.Join(dir2, "live.mfs")},
		{o1.Map, filepath.Join(dir2, "map.json")},
		{o1.BadCRC, filepath.Join(dir2, "bad-crc.mfs")},
		{o1.Truncated, filepath.Join(dir2, "truncated.mfs")},
		{o1.Conflict, filepath.Join(dir2, "conflict.mfs")},
	} {
		a, _ := os.ReadFile(pair[0])
		b, _ := os.ReadFile(pair[1])
		if string(a) != string(b) {
			t.Fatalf("non-deterministic output: %s", pair[0])
		}
	}
}

func TestLiveMFSIsValidAndContainsAllCameras(t *testing.T) {
	dir := t.TempDir()
	o, err := Generate(Plan{Dir: dir, Frames: 400, Faults: splitFaults("late,duplicate")})
	if err != nil {
		t.Fatal(err)
	}
	frames, err := demux.ReadAll(mustOpen(t, o.Live), "live.mfs")
	if err != nil {
		t.Fatalf("live.mfs must be fully valid: %v", err)
	}
	cams := map[uint16]bool{}
	for _, f := range frames {
		cams[f.CameraID] = true
	}
	if len(cams) != CameraCount {
		t.Fatalf("cameras = %d, want %d", len(cams), CameraCount)
	}
}

func TestBadCRCFaultIsLocated(t *testing.T) {
	dir := t.TempDir()
	o, _ := Generate(Plan{Dir: dir, Frames: 40, Faults: splitFaults("crc")})
	_, err := demux.ReadAll(mustOpen(t, o.BadCRC), "bad-crc.mfs")
	f := media.AsFault(err)
	if f == nil || f.Stage != media.StageCRC || f.OffsetErr != 0 {
		t.Fatalf("want precise CRC fault, got %v", err)
	}
}

func TestTruncatedFaultIsLocated(t *testing.T) {
	dir := t.TempDir()
	o, _ := Generate(Plan{Dir: dir, Frames: 40, Faults: splitFaults("truncated")})
	_, err := demux.ReadAll(mustOpen(t, o.Truncated), "truncated.mfs")
	f := media.AsFault(err)
	if f == nil || (f.Stage != media.StageLength && f.Stage != media.StagePrefix) {
		t.Fatalf("want length fault, got %v", err)
	}
}

func TestConflictFramesHaveDifferentHashes(t *testing.T) {
	dir := t.TempDir()
	o, _ := Generate(Plan{Dir: dir, Frames: 40, Faults: splitFaults("duplicate")})
	frames, err := demux.ReadAll(mustOpen(t, o.Conflict), "conflict.mfs")
	if err != nil || len(frames) != 2 {
		t.Fatalf("conflict sample: %v %d", err, len(frames))
	}
	if frames[0].SHA256 == frames[1].SHA256 {
		t.Fatal("conflict frames must differ in payload hash")
	}
}

func TestSlowSampleIsLargeValidRecord(t *testing.T) {
	dir := t.TempDir()
	o, _ := Generate(Plan{Dir: dir, Frames: 40, Faults: splitFaults("slow")})
	frames, err := demux.ReadAll(mustOpen(t, o.Slow), "slow.mfs")
	if err != nil || len(frames) != 1 || len(frames[0].Payload) < 1<<20 {
		t.Fatalf("slow sample: %v frames=%d", err, len(frames))
	}
}
