package samplegen_test

import (
	"os"
	"testing"

	gomp4 "github.com/abema/go-mp4"

	"mediascan/internal/samplegen"
)

func TestGoMP4ProbeCleanFixtures(t *testing.T) {
	set, err := samplegen.Generate(t.TempDir(), 17)
	if err != nil {
		t.Fatal(err)
	}
	for _, idx := range []int{0, 1, 2} {
		f, err := os.Open(set.Files[idx].Path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := gomp4.Probe(f)
		_ = f.Close()
		if err != nil {
			t.Fatalf("probe %s: %v", set.Files[idx].Name, err)
		}
		if len(info.Tracks) != 2 {
			t.Fatalf("%s tracks=%d, want 2", set.Files[idx].Name, len(info.Tracks))
		}
	}
}
