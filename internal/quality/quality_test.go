package quality

import "testing"

func TestPSNRAndRegression(t *testing.T) {
	a := []byte{0, 10, 20, 30}
	b := []byte{0, 10, 20, 30}
	if got := FramePSNR(a, b); got != perfectPSNR {
		t.Fatalf("perfect PSNR = %f", got)
	}
	c := []byte{0, 12, 18, 33}
	if got := FramePSNR(a, c); got <= 0 || got >= perfectPSNR {
		t.Fatalf("unexpected noisy PSNR %f", got)
	}
	result, err := Compare(
		[]SegmentQuality{{ID: 1, PSNRDB: 40}, {ID: 2, PSNRDB: 39}},
		[]SegmentQuality{{ID: 1, PSNRDB: 38.5}, {ID: 2, PSNRDB: 39}},
		0.8,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.RegressedCount != 1 || result.MaxDropDB != 1.5 || result.Verdict != "REGRESSION" {
		t.Fatalf("unexpected regression result: %+v", result)
	}
}
