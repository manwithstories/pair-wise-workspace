package hls

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPublishAndChecksumStable(t *testing.T) {
	dir := t.TempDir()
	files := []File{
		{RelPath: "master.m3u8", Content: []byte("#EXTM3U\n")},
		{RelPath: filepath.Join("v1", "media.m3u8"), Content: []byte("#EXT-X-ENDLIST\n")},
	}
	if err := Publish(dir, files); err != nil {
		t.Fatal(err)
	}
	// All files land at their final paths with exact content.
	for _, f := range files {
		got, err := os.ReadFile(filepath.Join(dir, f.RelPath))
		if err != nil {
			t.Fatalf("missing published file %s: %v", f.RelPath, err)
		}
		if string(got) != string(f.Content) {
			t.Fatalf("content mismatch for %s", f.RelPath)
		}
	}
	// No temp files survive a successful publish.
	matches, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if len(matches) != 0 {
		t.Fatalf("temp files left behind: %v", matches)
	}
	// Checksum is order-independent for the same file set.
	shuffled := []File{files[1], files[0]}
	if ManifestChecksum(files) != ManifestChecksum(shuffled) {
		t.Fatal("checksum must not depend on input slice order")
	}
	// A content change changes the digest.
	changed := []File{{RelPath: "master.m3u8", Content: []byte("#EXTM3U\nv2")}}
	if ManifestChecksum(changed) == ManifestChecksum(files[:1]) {
		t.Fatal("identical digest for different content")
	}
}

func TestChecksumBytesIsHex256(t *testing.T) {
	sum := ChecksumBytes([]byte("hello"))
	if len(sum) != 64 {
		t.Fatalf("digest length = %d, want 64 hex chars", len(sum))
	}
	for _, c := range sum {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("digest %q contains non-hex char %q", sum, c)
		}
	}
}
