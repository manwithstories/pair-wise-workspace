package index

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/abema/go-mp4"
)

func TestWriterProducesReadableTables(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.mp4")
	w, err := Create(out, filepath.Join(dir, "tbl"), 4)
	if err != nil {
		t.Fatal(err)
	}
	var media int64 = 90000
	for i := 0; i < 10; i++ {
		payload := make([]byte, 16)
		payload[0] = byte(i)
		if err := w.Append(uint64(i), media, payload); err != nil {
			t.Fatal(err)
		}
		if _, err := w.CloseChunk(); err != nil {
			t.Fatal(err)
		}
		media += 3000
	}
	if err := w.Finish(); err != nil {
		t.Fatal(err)
	}
	if err := w.Publish(); err != nil {
		t.Fatal(err)
	}

	f, ferr := os.Open(out)
	if ferr != nil {
		t.Fatal(ferr)
	}
	_, err = mp4.ReadBoxStructure(f, func(h *mp4.ReadHandle) (interface{}, error) {
		t.Logf("%s offset=%d size=%d", h.BoxInfo.Type, h.BoxInfo.Offset, h.BoxInfo.Size)
		return h.Expand()
	})
	t.Logf("structure err=%v", err)
	f.Close()

	rep, err := VerifyFile(out, VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("report %+v", rep)
	if !rep.IndexValid {
		t.Fatalf("invalid: %v", rep.Problems)
	}
	if rep.Samples != 10 {
		t.Fatalf("samples %d", rep.Samples)
	}
}
