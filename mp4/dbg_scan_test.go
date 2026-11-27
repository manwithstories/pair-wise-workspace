package mp4_test

import (
	"os"
	"testing"

	"github.com/fashionweek/mediad/internal/mp4"
)

func TestDBGBytes(t *testing.T) {
	data := buildMP4(t, 90000, true)
	p := writeTemp(t, "pts.mp4", data)
	f, _ := os.Open(p)
	defer f.Close()
	idx, err := mp4.IndexFile(f)
	if err != nil {
		t.Logf("err: %v", err)
		return
	}
	for i, s := range idx.Video.Samples {
		t.Logf("sample %d dts=%s pts=%s", i, s.DTS, s.PTS)
	}
}
