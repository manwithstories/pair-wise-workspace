package encode

import "testing"

func TestConfigHashStableAndSensitive(t *testing.T) {
	a := DefaultConfig(1200, 30)
	h1, err := a.SHA256()
	if err != nil || len(h1) != 64 {
		t.Fatalf("hash=%q err=%v", h1, err)
	}
	h2, _ := a.SHA256()
	b := a
	b.TargetKbps = 1500
	h3, _ := b.SHA256()
	if h1 != h2 || h1 == h3 {
		t.Fatal("config hash must be stable and change with bitrate")
	}
}

func TestControllerTargetsFivePercentWindow(t *testing.T) {
	r := NewController(DefaultConfig(1200, 30), 30)
	if r.TargetBytes != 4_500_000 {
		t.Fatalf("target bytes = %d", r.TargetBytes)
	}
	// 4.4 MB is about 1173 kbps, within 5%.
	if kbps := r.ActualKbps(4_400_000); kbps < 1140 || kbps > 1260 {
		t.Fatalf("kbps=%d outside expected demo window", kbps)
	}
	if !r.WithinTolerance(4_400_000) || r.WithinTolerance(4_000_000) {
		t.Fatal("tolerance calculation incorrect")
	}
	pad := r.NeededPadding(4_400_000)
	if pad != 99_992 { // target 4.5m - current 4.4m - free header 8
		t.Fatalf("padding = %d", pad)
	}
}
