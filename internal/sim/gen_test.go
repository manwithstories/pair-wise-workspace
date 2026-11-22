package sim

import (
	"testing"

	"mediaroute/internal/adapter"
)

func TestGenerateDeterministic(t *testing.T) {
	a := Generate(Options{Seed: 41, LossBurst: 7, Bandwidth: []int64{2e6, 8e5, 3e6}})
	b := Generate(Options{Seed: 41, LossBurst: 7, Bandwidth: []int64{2e6, 8e5, 3e6}})
	if len(a.Items) != len(b.Items) || len(a.Meta) != len(b.Meta) {
		t.Fatal("nondeterministic item count")
	}
	for i := range a.Items {
		if string(a.Items[i].Data) != string(b.Items[i].Data) || a.Items[i].TimeNs != b.Items[i].TimeNs {
			t.Fatalf("item %d differs", i)
		}
	}
	if len(a.Keyframes[PrimaryRendition]) != 30 {
		t.Fatalf("keyframes = %d, want 30", len(a.Keyframes[PrimaryRendition]))
	}
}

func TestGenerateLossBurst(t *testing.T) {
	s := Generate(Options{Seed: 41, LossBurst: 7})
	count := 0
	for _, it := range s.Items {
		if it.Track == "video-480p" {
			count++
		}
	}
	// 750 frames - 7 lost + 8 injected duplicates (every 97th of 743)
	if want := 750 - 7 + 8; count != want {
		t.Fatalf("480p packets = %d, want %d", count, want)
	}
}

func TestGenerateCorruptTruncatedBox(t *testing.T) {
	s := Generate(Options{Seed: 1, Corrupt: "truncated-box"})
	p := adapter.NewMP4Parser()
	errs := 0
	for _, it := range s.Items {
		if it.Parser != "mp4" {
			continue
		}
		if _, err := p.Parse(it.Track, it.Data, it.Offset); err != nil {
			errs++
		}
	}
	if errs != 1 {
		t.Fatalf("parse errors = %d, want 1", errs)
	}
}

func TestGenerateRTPDecodable(t *testing.T) {
	s := Generate(Options{Seed: 41, LossBurst: 7})
	p := adapter.NewRTPParser(ClockRate, BaseTick)
	seen := 0
	for _, it := range s.Items {
		if it.Parser != "rtp" {
			continue
		}
		evs, err := p.Parse(it.Track, it.Data, it.Offset)
		if err != nil {
			t.Fatalf("rtp parse: %v", err)
		}
		if evs[0].TimestampNs < 0 || evs[0].TimestampNs > s.DurationNs {
			t.Fatalf("ts out of range: %d", evs[0].TimestampNs)
		}
		seen++
	}
	if seen == 0 {
		t.Fatal("no rtp items")
	}
}
