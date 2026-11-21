package pairflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/mediamend/mediamend/internal/fault"
	"github.com/mediamend/mediamend/internal/frame"
)

func TestGenerateDeterministic(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	ma, fa := Generate(Options{Root: a, Frames: 120, CorruptEvery: 17, CameraCount: 2})
	if fa.Fatal() {
		t.Fatalf("faults=%v", fa)
	}
	mb, fb := Generate(Options{Root: b, Frames: 120, CorruptEvery: 17, CameraCount: 2})
	if fb.Fatal() {
		t.Fatalf("faults=%v", fb)
	}
	if len(ma.Cameras) != 2 || len(mb.Cameras) != 2 {
		t.Fatalf("camera count %d", len(ma.Cameras))
	}
	for _, cam := range ma.Cameras {
		ra, err := os.ReadFile(filepath.Join(a, "input", cam.ID+".frm"))
		if err != nil {
			t.Fatal(err)
		}
		rb, err := os.ReadFile(filepath.Join(b, "input", cam.ID+".frm"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(ra, rb) {
			t.Fatalf("%s not deterministic across runs", cam.ID)
		}
	}
	if ma.TotalCorrupt() != mb.TotalCorrupt() || ma.TotalCorrupt() == 0 {
		t.Fatalf("corrupt seqs differ: %d vs %d", ma.TotalCorrupt(), mb.TotalCorrupt())
	}
}

func TestGenerateRecordsParse(t *testing.T) {
	dir := t.TempDir()
	m, faults := Generate(Options{Root: dir, Frames: 90, CorruptEvery: 0, CameraCount: 1})
	if faults.Fatal() {
		t.Fatalf("faults=%v", faults)
	}
	cam := m.Cameras[0]
	f, err := os.Open(filepath.Join(dir, "input", cam.ID+".frm"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r := frame.NewReader(f, cam.ID, 0)
	var n int
	for {
		rec, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("record %d: %v", n, err)
		}
		if len(rec.Payload) == 0 {
			t.Fatalf("record %d has no payload", n)
		}
		if rec.Seq != uint64(n) {
			t.Fatalf("record %d seq=%d", n, rec.Seq)
		}
		if rec.Keyframe() != (n%30 == 0) {
			t.Fatalf("record %d keyframe=%v", n, rec.Keyframe())
		}
		n++
	}
	if n != int(cam.Frames) {
		t.Fatalf("records=%d want %d", n, cam.Frames)
	}
}

func TestCorruptSeqsMatchMetadata(t *testing.T) {
	dir := t.TempDir()
	m, faults := Generate(Options{Root: dir, Frames: 100, CorruptEvery: 13, CameraCount: 1})
	if faults.Fatal() {
		t.Fatalf("faults=%v", faults)
	}
	cam := m.Cameras[0]
	md, err := frame.LoadMetadata(filepath.Join(dir, "input", cam.ID+".meta"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[uint64]bool{}
	for _, s := range m.CorruptSeqs[cam.ID] {
		want[s] = true
	}
	if len(want) != len(CorruptSeq(100, 13)) {
		t.Fatalf("corrupt list=%v", want)
	}
	for seq := uint64(0); seq < 100; seq++ {
		e, ok := md.Lookup(cam.ID, seq)
		if !ok {
			t.Fatalf("metadata missing seq %d", seq)
		}
		bad := e.SHA256 == "0000000000000000000000000000000000000000000000000000000000000000"
		if bad != want[seq] {
			t.Fatalf("seq %d corrupted=%v want %v", seq, bad, want[seq])
		}
	}
}

func TestGenerateDefectSamples(t *testing.T) {
	dir := t.TempDir()
	m, faults := Generate(Options{Root: dir, Frames: 60, CameraCount: 1})
	if faults.Fatal() {
		t.Fatalf("faults=%v", faults)
	}
	samples := m.Files["boundaries"]
	if samples == "" {
		t.Fatal("boundary samples not recorded")
	}
	for _, name := range []string{"truncated.frm", "checksum.frm", "wrap.frm", "duplicate.frm"} {
		if _, err := os.Stat(filepath.Join(dir, "boundaries", name)); err != nil {
			t.Fatalf("sample %s missing: %v", name, err)
		}
	}
}

func TestGenerateBatchResetAndWrap(t *testing.T) {
	dir := t.TempDir()
	m, faults := Generate(Options{Root: dir, Frames: 200, CameraCount: 6})
	if faults.Fatal() {
		t.Fatalf("faults=%v", faults)
	}
	// camB declares a batch reset; its frame file must contain a low
	// sequence after the reset point.
	camB := m.Cameras[1]
	if camB.BatchResetAt == 0 {
		t.Fatal("camB has no batch reset")
	}
	f, err := os.Open(filepath.Join(dir, "input", camB.ID+".frm"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r := frame.NewReader(f, camB.ID, 0)
	var seqs []uint64
	for {
		rec, err := r.Next()
		if err != nil {
			break
		}
		seqs = append(seqs, rec.Seq)
	}
	found := false
	for i := 1; i < len(seqs); i++ {
		if seqs[i] < seqs[i-1] {
			found = true
			if seqs[i] != 0 {
				t.Fatalf("reset should restart at 0, got %d", seqs[i])
			}
		}
	}
	if !found {
		t.Fatal("batch reset not present in camB")
	}
}

func TestGenerateRequiresRoot(t *testing.T) {
	if _, faults := Generate(Options{}); !faults.Fatal() {
		t.Fatalf("missing root accepted: %v", faults)
	}
}

func unmarshalJSON(data []byte, v any) error { return json.Unmarshal(data, v) }

func TestBoundarySamplesTruncated(t *testing.T) {
	dir := t.TempDir()
	m, _ := Generate(Options{Root: dir, Frames: 30, CameraCount: 1})
	path := m.Files["boundaries"]
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var samples BoundarySamples
	if err := unmarshalJSON(data, &samples); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(samples.Truncated)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r := frame.NewReader(f, "check", 0)
	for {
		_, err := r.Next()
		if err == nil {
			continue
		}
		if !errors.Is(err, frame.ErrBadFraming) {
			t.Fatalf("want framing error, got %v", err)
		}
		break
	}
	fm, ok := r.Faults().FirstOf(fault.KindTruncatedRecord)
	if !ok || fm.Offset != int64(len(mustRecords(t, 2))) {
		t.Fatalf("truncated fault=%+v ok=%v", fm, ok)
	}
}

// mustRecords returns the encoded length of n synthetic records matching the
// boundary sample layout.
func mustRecords(t *testing.T, n int) []byte {
	t.Helper()
	var out []byte
	for seq := uint64(0); seq < uint64(n); seq++ {
		out = frame.Encode(out, frame.Header{
			Camera: "check", Seq: seq, DeviceTS: seq * 3000, Rate: 90000,
		}, []byte{byte(seq), byte(seq >> 8), 0xAA, 0x55})
	}
	return out
}
