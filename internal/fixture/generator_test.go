package fixture

import (
	"bytes"
	"testing"

	"medpipe/internal/model"
	"medpipe/internal/scan"
)

func TestGenerateScansClean(t *testing.T) {
	raw, err := Generate(Options{Seed: 42, Frames: 60})
	if err != nil {
		t.Fatal(err)
	}
	rd, err := scan.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	res, err := rd.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Frames) != 60 || len(res.Audio) != 120 {
		t.Fatalf("counts frames=%d audio=%d", len(res.Frames), len(res.Audio))
	}
	if res.FirstCorruption != nil || res.TailTruncated {
		t.Fatal("fresh fixture must be clean")
	}
}

func TestGenerateDeterministic(t *testing.T) {
	a, _ := Generate(Options{Seed: 9, Frames: 30})
	b, _ := Generate(Options{Seed: 9, Frames: 30})
	if !bytes.Equal(a, b) {
		t.Fatal("fixture generation must be deterministic")
	}
	// Video payloads repeat on purpose for dedup; distinct seeds still differ
	// through the generated audio tone placement (i%8) and container counts.
	c, _ := Generate(Options{Seed: 10, Frames: 31})
	if bytes.Equal(a, c) {
		t.Fatal("different fixture parameters must produce different containers")
	}
}

func TestCorruptFrameReportedAtOffset(t *testing.T) {
	raw, _ := Generate(Options{Seed: 1, Frames: 40})
	off, err := CorruptFrame(raw, 17)
	if err != nil {
		t.Fatal(err)
	}
	rd, _ := scan.NewReader(bytes.NewReader(raw))
	res, err := rd.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if res.FirstCorruption == nil {
		t.Fatal("corruption not detected")
	}
	if res.FirstCorruption.Offset != off || res.FirstCorruption.Seq != 17 {
		t.Fatalf("want offset=%d seq=17, got %+v", off, res.FirstCorruption)
	}
	if res.FirstCorruption.Code != model.ErrChecksum {
		t.Fatalf("code=%s", res.FirstCorruption.Code)
	}
}

func TestTruncateProducesRecoverableTail(t *testing.T) {
	raw, _ := Generate(Options{Seed: 1, Frames: 20})
	cut := Truncate(append([]byte(nil), raw...), 9)
	rd, _ := scan.NewReader(bytes.NewReader(cut))
	res, err := rd.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if !res.TailTruncated || res.FirstCorruption != nil {
		t.Fatalf("truncation handling wrong: trunc=%v corr=%v", res.TailTruncated, res.FirstCorruption)
	}
	if res.CompleteRecords >= len(raw) {
		t.Fatal("at least the final record must be dropped")
	}
}
