package checksum

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestFileAndPrefixAgree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blob.bin")
	data := make([]byte, 1024)
	for i := range data {
		data[i] = byte(i * 7)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	full, size, err := File(path)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(data)) || len(full) != 64 {
		t.Fatalf("size=%d sha=%q", size, full)
	}
	prefix, err := Prefix(path, 512)
	if err != nil {
		t.Fatal(err)
	}
	if prefix == full {
		t.Fatal("prefix digest equals full digest")
	}
	// A prefix of the whole file must equal the whole-file digest.
	all, err := Prefix(path, size)
	if err != nil {
		t.Fatal(err)
	}
	if all != full {
		t.Fatalf("prefix(all)=%s file=%s", all, full)
	}
}

func TestSampleHasherFramesLengths(t *testing.T) {
	a := NewSampleHasher()
	a.Add([]byte("ab"))
	a.Add([]byte("c"))
	b := NewSampleHasher()
	b.Add([]byte("a"))
	b.Add([]byte("bc"))
	if a.Sum() == b.Sum() {
		t.Fatal("length framing collapsed distinct sample sequences")
	}
	if a.Count() != 2 || a.Bytes() != 3 {
		t.Fatalf("count=%d bytes=%d", a.Count(), a.Bytes())
	}
	c := NewSampleHasher()
	c.Add([]byte("ab"))
	c.Add([]byte("c"))
	if a.Sum() != c.Sum() {
		t.Fatal("identical sequences hash differently")
	}
}

func TestReaderDigest(t *testing.T) {
	got, err := Reader(bytes.NewReader([]byte("mediamerge")))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 64 {
		t.Fatalf("digest=%q", got)
	}
	file, _, err := File(writeTemp(t, "mediamerge"))
	if err != nil {
		t.Fatal(err)
	}
	if file != got {
		t.Fatalf("reader digest %s != file digest %s", got, file)
	}
}

func writeTemp(t *testing.T, s string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
