package index

import (
	"os"
	"testing"
)

// TestVerifyEntryFailures 覆盖索引首尾、逆序与偏移非单调三类校验失败。
func TestVerifyEntryFailures(t *testing.T) {
	good := []Entry{
		{TrackID: 1, StartSample: 0, SampleCount: 10, DataOffset: 16, DataLength: 20, RTPSeq: 0},
		{TrackID: 1, StartSample: 10, SampleCount: 10, DataOffset: 36, DataLength: 20, RTPSeq: 1},
	}
	if se := VerifyEntries(1, good); se != nil {
		t.Fatalf("good entries rejected: %v", se)
	}
	if se := VerifyEntries(1, nil); se == nil {
		t.Fatal("empty index must fail verification")
	}
	badFirst := []Entry{{TrackID: 1, StartSample: 5, SampleCount: 1, DataOffset: 16}}
	if se := VerifyEntries(1, badFirst); se == nil {
		t.Fatal("non-zero first sample must fail")
	}
	badOverlap := []Entry{
		{TrackID: 1, StartSample: 0, SampleCount: 10, DataOffset: 16},
		{TrackID: 1, StartSample: 9, SampleCount: 10, DataOffset: 36},
	}
	if se := VerifyEntries(1, badOverlap); se == nil {
		t.Fatal("overlapping entries must fail")
	}
	badOffset := []Entry{
		{TrackID: 1, StartSample: 0, SampleCount: 10, DataOffset: 36},
		{TrackID: 1, StartSample: 10, SampleCount: 10, DataOffset: 16},
	}
	if se := VerifyEntries(1, badOffset); se == nil {
		t.Fatal("non-monotonic data offset must fail")
	}
}

// TestSeekReadbackMismatch 断言损坏后的跳转读回与原 PCM 不一致会被捕获。
func TestSeekReadbackMismatch(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "idx-*.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := NewBuilder(f)
	if err := b.Begin(make([]byte, 16)); err != nil {
		t.Fatal(err)
	}
	pcm := []byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x11, 0x22, 0x33}
	off, length, err := b.AddPCM(pcm)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Finalize([]Descriptor{{TrackID: 1, Entries: []Entry{
		{TrackID: 1, StartSample: 0, SampleCount: 4, DataOffset: off, DataLength: length},
	}}}); err != nil {
		t.Fatal(err)
	}
	rf, err := os.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	entries, mdatBase, _, err := ReadFile(rf)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ReadAt(rf, mdatBase, entries[1][0].DataOffset, int(length))
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != string(pcm) {
		t.Fatalf("seek readback mismatch: %x", back)
	}
	// 故意错位一字节读取，必须读回不同内容。
	shifted, err := ReadAt(rf, mdatBase, entries[1][0].DataOffset+1, int(length)-1)
	if err != nil {
		t.Fatal(err)
	}
	if string(shifted) == string(pcm[:length-1]) {
		t.Fatal("shifted read unexpectedly matched original bytes")
	}
}
