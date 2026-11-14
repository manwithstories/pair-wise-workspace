package report

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestBuildOrderAndJSON(t *testing.T) {
	rep := Build(1_500_000, 18_000_000, 12, []SegmentReport{
		{ID: 2, Bits: 500_000, BPS: 500_000, QP: 30, PSNRDB: 39, CRC: 22},
		{ID: 1, Bits: 600_000, BPS: 600_000, QP: 29, PSNRDB: 41, CRC: 11},
	})
	if rep.Segments[0].ID != 1 || rep.TotalBits != 1_100_000 || rep.SegmentCount != 2 {
		t.Fatalf("unexpected report: %+v", rep)
	}
	data, err := rep.Encode()
	if err != nil {
		t.Fatal(err)
	}
	order := [][]byte{
		[]byte(`"target_bps"`), []byte(`"actual_bps"`), []byte(`"deviation_pct"`),
		[]byte(`"psnr_mean_db"`), []byte(`"psnr_segment_std_db"`), []byte(`"segments"`),
	}
	last := -1
	for _, key := range order {
		index := bytes.Index(data, key)
		if index <= last {
			t.Fatalf("field order is not stable at %s", key)
		}
		last = index
	}
	var decoded Report
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Segments[0].ID != 1 || decoded.TargetBPS != 1_500_000 {
		t.Fatalf("round trip mismatch: %+v", decoded)
	}
}
