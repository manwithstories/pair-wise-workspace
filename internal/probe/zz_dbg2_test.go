package probe

import (
	"path/filepath"
	"testing"
)

func TestZZDbg2(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "r.mp4")
	writeSttsFixture(t, path, []pair{{4, 512}}, []int32{2000, 0, 1000, 0})
	f, err := Analyze(path, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	tr := f.Tracks[0]
	t.Logf("declared=%d walked=%d issues=%v idx=%+v table=%+v", tr.Declared, tr.Walked, tr.Issues, tr.Index, tr.table)
	cur := tr.Cursor()
	for {
		row, ok := cur.Next()
		if !ok {
			break
		}
		t.Logf("row %d dts=%d cts=%d dtsms=%d ctsms=%d", row.Index, row.DTS, row.CTS, row.DTSMS, row.CTSMS)
	}
}
