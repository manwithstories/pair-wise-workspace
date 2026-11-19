package index

import (
	"bytes"
	"fmt"
	"path/filepath"
	"testing"
)

func payloadFor(seq uint64, n int) []byte {
	b := bytes.Repeat([]byte{byte(seq), byte(seq >> 8), 0xAA, 0x55}, n/4+1)
	return b[:n]
}

// buildPart writes n samples into out.part starting at startSeq-1. When st is
// nil it creates a fresh part; otherwise it resumes. With final=true the part
// is finished and published.
func buildPart(t *testing.T, out, tbl string, st *State, startSeq uint64, n int, final bool) *State {
	t.Helper()
	var w *Writer
	var err error
	if st == nil {
		w, err = Create(out, tbl, 8)
	} else {
		w, err = Resume(out, tbl, 8, *st)
	}
	if err != nil {
		t.Fatal(err)
	}
	seq := startSeq
	media := int64(0)
	if st != nil {
		seq = st.LastSeq
		media = st.LastMedia
	}
	for i := 0; i < n; i++ {
		seq++
		media += 3000
		if err := w.Append(seq, media, payloadFor(seq, 64)); err != nil {
			t.Fatal(err)
		}
		if w.sampleCount%8 == 0 {
			if _, err := w.CloseChunk(); err != nil {
				t.Fatal(err)
			}
		}
	}
	s, err := w.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if final {
		if err := w.Finish(); err != nil {
			t.Fatal(err)
		}
		if err := w.Publish(); err != nil {
			t.Fatal(err)
		}
	} else if err := w.CloseWithoutPublishing(); err != nil {
		t.Fatal(err)
	}
	return &s
}

func TestResumeMatchesClean(t *testing.T) {
	dir := t.TempDir()
	cleanOut := filepath.Join(dir, "clean.mp4")
	buildPart(t, cleanOut, filepath.Join(dir, "ta"), nil, 0, 40, true)

	resOut := filepath.Join(dir, "res.mp4")
	tbl := filepath.Join(dir, "tb")
	st := buildPart(t, resOut, tbl, nil, 0, 13, false)
	st = buildPart(t, resOut, tbl, st, 0, 20, false)
	buildPart(t, resOut, tbl, st, 0, 7, true)

	cleanRep, err := VerifyFile(cleanOut, VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	resRep, err := VerifyFile(resOut, VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !cleanRep.IndexValid || !resRep.IndexValid {
		t.Fatalf("clean=%v res=%v", cleanRep.Problems, resRep.Problems)
	}
	if cleanRep.Samples != resRep.Samples || cleanRep.LastMedia != resRep.LastMedia {
		t.Fatalf("clean=%+v res=%+v", cleanRep, resRep)
	}
	ct := mustTable(t, cleanOut)
	rt := mustTable(t, resOut)
	if fmt.Sprint(ct.SampleSize) != fmt.Sprint(rt.SampleSize) {
		t.Fatal("sample sizes differ")
	}
	if fmt.Sprint(ct.Offsets) != fmt.Sprint(rt.Offsets) {
		t.Fatal("sample offsets differ")
	}
	if fmt.Sprint(ct.Times) != fmt.Sprint(rt.Times) {
		t.Fatal("sample times differ")
	}
}

func mustTable(t *testing.T, p string) *Table {
	t.Helper()
	tb, err := ReadTable(p)
	if err != nil {
		t.Fatal(err)
	}
	return tb
}
