package parse

import (
	"os"
	"path/filepath"
	"testing"

	"archivecheck/internal/fault"
	"archivecheck/internal/testkit"
)

func stdFiles(t *testing.T) (string, *testkit.Manifest) {
	t.Helper()
	init, frags := testkit.StandardSampleLayout()
	m, err := testkit.Generate(t.TempDir(), init, frags)
	if err != nil {
		t.Fatal(err)
	}
	return m.Dir, m
}

func open(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestParseInit(t *testing.T) {
	dir, _ := stdFiles(t)
	init, err := ParseInit(open(t, filepath.Join(dir, "init.mp4")), "init.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if init.Brand != "iso6" {
		t.Fatalf("brand = %q", init.Brand)
	}
	tr := init.Tracks[1]
	if tr == nil || tr.Timescale != 90000 || tr.Handler != "vide" {
		t.Fatalf("track = %+v", tr)
	}
	if len(init.Order) != 1 || init.Order[0] != 1 {
		t.Fatalf("order = %v", init.Order)
	}
}

func TestParseFragmentSamples(t *testing.T) {
	dir, _ := stdFiles(t)
	init, err := ParseInit(open(t, filepath.Join(dir, "init.mp4")), "init.mp4")
	if err != nil {
		t.Fatal(err)
	}
	frag, err := ParseFragment(open(t, filepath.Join(dir, "frag-001.m4s")), "frag-001.m4s", init)
	if err != nil {
		t.Fatal(err)
	}
	if !frag.HasMfhd || frag.Sequence != 1 {
		t.Fatalf("mfhd = %+v", frag)
	}
	if len(frag.Trafs) != 1 || len(frag.Trafs[0].Samples) != 4 {
		t.Fatalf("trafs = %+v", frag.Trafs)
	}
	tr := frag.Trafs[0]
	if !tr.HasTfdt || tr.BaseDecodeTime != 0 {
		t.Fatalf("tfdt = %+v", tr)
	}
	total := uint64(0)
	for i, s := range tr.Samples {
		if s.Offset%1 != 0 || s.Size != 512 || s.Duration != 3000 {
			t.Fatalf("sample %d = %+v", i, s)
		}
		if s.Sync() != (i == 0) {
			t.Fatalf("sample %d sync = %v", i, s.Sync())
		}
		total += uint64(s.Size)
	}
	if int64(total) != frag.MdatEnd-frag.MdatPayload {
		t.Fatalf("sample bytes %d != mdat payload %d", total, frag.MdatEnd-frag.MdatPayload)
	}
}

func TestParseFragmentMissingTfdt(t *testing.T) {
	dir, m := stdFiles(t)
	init, _ := ParseInit(open(t, filepath.Join(dir, "init.mp4")), "init.mp4")
	req := m.Segments[1]
	data, err := os.ReadFile(req.Path)
	if err != nil {
		t.Fatal(err)
	}
	// Strip the tfdt box from the moof by overwriting "tfdt" with "free".
	idx := indexOf(data, []byte("tfdt"))
	if idx < 4 {
		t.Fatal("tfdt not found")
	}
	copy(data[idx:], []byte("free"))
	p := filepath.Join(t.TempDir(), "no-tfdt.m4s")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = ParseFragment(open(t, p), "no-tfdt.m4s", init)
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindMissingBox || fe.Segment != "no-tfdt.m4s" {
		t.Fatalf("err = %v", err)
	}
}

func indexOf(haystack, needle []byte) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
