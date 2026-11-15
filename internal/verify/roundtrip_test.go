package verify

import (
	"math/big"
	"testing"

	"subtool/internal/encoding"
	"subtool/internal/model"
	"subtool/internal/serialize"
)

func sampleCues() []model.Cue {
	return []model.Cue{
		{ID: "0001", Start: big.NewRat(1000, 1), End: big.NewRat(2500, 1),
			Text: "hello <i>world</i>", Tags: []string{"<i>", "</i>"}},
		{ID: "0002", Start: big.NewRat(3000, 1), End: big.NewRat(4200, 1),
			Text: "第二行字幕"},
		{ID: "0003", Start: big.NewRat(5000, 1), End: big.NewRat(6001, 1),
			Text: "{\\an8}顶部对白"},
	}
}

// Every format must round-trip through the shared parse layer with
// sub-millisecond drift and zero cue loss.
func TestRoundtripAllFormats(t *testing.T) {
	for _, f := range serialize.Formats {
		res, err := Roundtrip(sampleCues(), f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if res.DroppedCues != 0 || res.AddedCues != 0 {
			t.Errorf("%s: dropped=%d added=%d", f, res.DroppedCues, res.AddedCues)
		}
		if res.TextMismatch != 0 {
			t.Errorf("%s: text mismatch=%d", f, res.TextMismatch)
		}
		if res.MaxDriftMs > 10 { // ASS centisecond precision = up to 10ms
			t.Errorf("%s: max drift %.3f ms", f, res.MaxDriftMs)
		}
		if f != serialize.ASS && res.MaxDriftMs > 0.5 {
			t.Errorf("%s: drift %.3f exceeds ms precision", f, res.MaxDriftMs)
		}
	}
}

func TestSniffEncodings(t *testing.T) {
	if r := encoding.Sniff([]byte("plain utf-8 text")); r.Name != "UTF-8" {
		t.Errorf("utf-8: got %s", r.Name)
	}
	bom := append([]byte{0xEF, 0xBB, 0xBF}, []byte("bom text")...)
	if r := encoding.Sniff(bom); r.Name != "UTF-8-BOM" {
		t.Errorf("bom: got %s", r.Name)
	}
	// UTF-16LE with BOM: "AB"
	u16 := []byte{0xFF, 0xFE, 'A', 0, 'B', 0}
	if r := encoding.Sniff(u16); r.Name != "UTF-16LE" || r.Text != "AB" {
		t.Errorf("utf16le: got %s %q", r.Name, r.Text)
	}
	// GB18030: "中" = 0xD6 0xD0
	gb := []byte{0xD6, 0xD0}
	if r := encoding.Sniff(gb); r.Name != "GB18030" || r.Text != "中" {
		t.Errorf("gb18030: got %s %q", r.Name, r.Text)
	}
}
