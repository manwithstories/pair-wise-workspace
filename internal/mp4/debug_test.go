package mp4

import (
	"bytes"
	"io"
	"testing"

	"mediad/internal/index"
)

func TestBuiltSyncSamplesContainVerifiableMagic(t *testing.T) {
	p := testProject()
	data, err := Build(p)
	if err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(data)
	got, _, _, err := ReadProject(r, int64(len(data)), false)
	if err != nil {
		t.Fatal(err)
	}
	idx, _ := index.Rebuild(got)
	misses, faults := idx.Verify(bytes.NewReader(data), int64(len(data)))
	if misses != 0 || len(faults) != 0 {
		t.Fatalf("misses=%d faults=%#v", misses, faults)
	}
	s := got.Tracks[0].Samples[0]
	if _, err := r.Seek(s.Offset, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(index.SyncMagic))
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != index.SyncMagic {
		t.Fatalf("magic=%q", buf)
	}
}
