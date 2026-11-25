package audit

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCompareFields(t *testing.T) {
	orig := []string{"a=1", "b=2", "c=3"}
	same := []string{"a=1", "b=2", "c=3"}
	rt := CompareFields(orig, same)
	if rt.Rate() != 1 || rt.Compared != 3 || rt.Matched != 3 {
		t.Fatalf("identical tables: %+v", rt)
	}
	diff := CompareFields(orig, []string{"a=1", "b=9"})
	if rt := diff.Rate(); rt != 1.0/3.0 {
		t.Fatalf("rate = %v, want 1/3", rt)
	}
	if len(diff.Missing) != 1 || len(diff.Differs) != 1 {
		t.Fatalf("diff = %+v", diff)
	}
	if got := CompareFields(nil, nil).Rate(); got != 1 {
		t.Fatalf("empty comparison rate = %v, want 1", got)
	}
}

func TestHashShardsDeterministic(t *testing.T) {
	data := []byte("the quick brown fox jumps over the lazy dog")
	a := HashShards(data, 4)
	b := HashShards(data, 4)
	if len(a) != 4 || len(b) != 4 {
		t.Fatalf("shard counts = %d/%d", len(a), len(b))
	}
	var total int64
	for i := range a {
		if a[i].Hash != b[i].Hash {
			t.Fatalf("shard %d differs", i)
		}
		if len(a[i].Hash) != 64 {
			t.Fatalf("shard %d hash is not 64 hex chars: %q", i, a[i].Hash)
		}
		total += a[i].Bytes
	}
	if total != int64(len(data)) {
		t.Fatalf("shards cover %d of %d bytes", total, len(data))
	}
}

func TestSplitSegments(t *testing.T) {
	hashes := make([]string, 120)
	for i := range hashes {
		hashes[i] = HashBytes([]byte{byte(i)})
	}
	segs := SplitSegments(hashes, 6)
	if len(segs) != 6 {
		t.Fatalf("segments = %d, want 6", len(segs))
	}
	covered := 0
	for _, s := range segs {
		covered += s.Last - s.First + 1
		if len(s.Hash) != 64 {
			t.Fatalf("segment hash not 64 hex: %q", s.Hash)
		}
	}
	if covered != 120 {
		t.Fatalf("segments cover %d frames", covered)
	}
	// Identical input yields identical segment digests.
	again := SplitSegments(hashes, 6)
	for i := range segs {
		if segs[i].Hash != again[i].Hash {
			t.Fatalf("segment %d not deterministic", i)
		}
	}
}

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.png"), []byte("aaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.png"), []byte("bbb"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err := BuildManifest(dir, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(lines) != 2 || lines[0].Name != "a.png" {
		t.Fatalf("manifest = %+v", lines)
	}
	path := filepath.Join(dir, "SHA256SUMS")
	if err := WriteManifest(path, lines); err != nil {
		t.Fatalf("write: %v", err)
	}
	back, err := ReadManifest(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if diff := CompareManifests(lines, back); len(diff) != 0 {
		t.Fatalf("manifest round trip differs: %v", diff)
	}
	// A changed file must show up as a difference.
	lines[0].Hash = HashBytes([]byte("changed"))
	if diff := CompareManifests(lines, back); len(diff) != 1 || diff[0] != "a.png" {
		t.Fatalf("change not detected: %v", diff)
	}
}

func TestFormatRate(t *testing.T) {
	for _, c := range []struct {
		m, t int
		want string
	}{
		{10, 10, "1.000"},
		{0, 10, "0.000"},
		{1, 3, "0.333"},
		{2, 3, "0.667"},
		{0, 0, "1.000"},
	} {
		if got := FormatRate(c.m, c.t); got != c.want {
			t.Fatalf("FormatRate(%d,%d) = %s, want %s", c.m, c.t, got, c.want)
		}
	}
}

func TestHashBytesKnownVector(t *testing.T) {
	// SHA-256 of the empty string is a well-known vector.
	if got := HashBytes(nil); got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("empty hash = %s", got)
	}
}
