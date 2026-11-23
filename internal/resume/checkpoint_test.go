package resume_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"media-pipeline/internal/fault"
	"media-pipeline/internal/resume"
)

func sampleCP() resume.Checkpoint {
	return resume.Checkpoint{
		ParamDigest: "params-abc",
		Sources: []resume.SourceRef{
			{BlockID: "b0", Sha256: hex.EncodeToString(make([]byte, 32))},
			{BlockID: "b1", Sha256: hex.EncodeToString(bytesOf(1))},
		},
		Segments: []resume.SegmentRef{
			{Index: 0, FirstSample: 0, EndSample: 10, Sha256: hex.EncodeToString(bytesOf(7))},
		},
		NextSample: 10,
	}
}

func bytesOf(b byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = b
	}
	return out
}

func TestAtomicWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cp := sampleCP()
	if err := resume.Write(dir, cp); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "checkpoint.json.tmp")); !os.IsNotExist(err) {
		t.Fatal("temporary checkpoint file must not remain after atomic rename")
	}
	got, err := resume.Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ParamDigest != cp.ParamDigest || got.NextSample != 10 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if got.SelfSha256 == "" {
		t.Fatal("self checksum not recorded")
	}
}

func TestReadMissingReturnsNil(t *testing.T) {
	got, err := resume.Read(t.TempDir())
	if err != nil || got != nil {
		t.Fatalf("missing checkpoint = (%v,%v), want nil,nil", got, err)
	}
}

func TestTamperedCheckpointRejected(t *testing.T) {
	dir := t.TempDir()
	if err := resume.Write(dir, sampleCP()); err != nil {
		t.Fatal(err)
	}
	p := resume.Path(dir)
	raw, _ := os.ReadFile(p)
	raw[len(raw)-5] = '9' // 篡改 JSON 末尾字符
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := resume.Read(dir)
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindCheckpointConflict {
		t.Fatalf("tamper err = %v, want checkpoint conflict", err)
	}
}

func TestVerifyParamDigestMismatch(t *testing.T) {
	cp := sampleCP()
	idx, err := resume.Verify(&cp, "different-params", cp.Sources)
	if fe, ok := fault.As(err); !ok || fe.Kind != fault.KindCheckpointConflict || idx != -1 {
		t.Fatalf("param verify = idx %d err %v, want conflict at -1", idx, err)
	}
}

func TestVerifySourceHashFirstMismatchPosition(t *testing.T) {
	cp := sampleCP()
	sources := append([]resume.SourceRef(nil), cp.Sources...)
	sources[1].Sha256 = hex.EncodeToString(bytesOf(9)) // 第二个源块哈希变了
	idx, err := resume.Verify(&cp, cp.ParamDigest, sources)
	if idx != 1 {
		t.Fatalf("mismatch index = %d, want 1", idx)
	}
	if fe, ok := fault.As(err); !ok || fe.Kind != fault.KindCheckpointConflict {
		t.Fatalf("err = %v, want conflict", err)
	}
}

func TestVerifySegmentChecksum(t *testing.T) {
	cp := sampleCP()
	good := sha256.Sum256(nil)
	copy(good[:], bytesOf(7)) // 与检查点一致
	if _, err := resume.VerifySegment(&cp, 0, good); err != nil {
		t.Fatalf("matching segment rejected: %v", err)
	}
	bad := sha256.Sum256([]byte("different"))
	idx, err := resume.VerifySegment(&cp, 0, bad)
	if idx != 0 {
		t.Fatalf("segment conflict index = %d, want 0", idx)
	}
	if fe, ok := fault.As(err); !ok || fe.Kind != fault.KindCheckpointConflict {
		t.Fatalf("err = %v, want conflict", err)
	}
}

func TestDeterministicRepeatedWrite(t *testing.T) {
	dir1, dir2 := t.TempDir(), t.TempDir()
	cp := sampleCP()
	if err := resume.Write(dir1, cp); err != nil {
		t.Fatal(err)
	}
	if err := resume.Write(dir2, cp); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(resume.Path(dir1))
	b, _ := os.ReadFile(resume.Path(dir2))
	if string(a) != string(b) {
		t.Fatal("repeated checkpoint writes are not byte-identical")
	}
}
