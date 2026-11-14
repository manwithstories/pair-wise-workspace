package fixture

import (
	"testing"
	"time"
)

func TestGenerateIsDeterministic(t *testing.T) {
	options := Options{Packets: 3000, Duration: 6 * time.Second, Seed: 42, Impairment: Impairment{Loss: 0.01, Duplicate: 0.01, Reorder: 0.02}}
	first, err := Generate(options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Generate(options)
	if err != nil {
		t.Fatal(err)
	}
	if first.Lost != second.Lost || first.Duplicated != second.Duplicated || first.Reordered != second.Reordered || len(first.Events) != len(second.Events) {
		t.Fatalf("counts differ: %+v vs %+v", first, second)
	}
	if first.Lost != 32 || first.Duplicated != 32 {
		t.Fatalf("seed 42 counts lost=%d duplicated=%d, want 32/32", first.Lost, first.Duplicated)
	}
	if len(first.Events) != options.Packets-first.Lost+first.Duplicated {
		t.Fatalf("events = %d", len(first.Events))
	}
	for i := range first.Events {
		a, b := first.Events[i], second.Events[i]
		if a.At != b.At || a.Packet.SequenceNumber != b.Packet.SequenceNumber || a.Packet.Timestamp != b.Packet.Timestamp || a.Duplicate != b.Duplicate {
			t.Fatalf("event %d differs", i)
		}
		if i > 0 && first.Events[i-1].At > a.At {
			t.Fatalf("event %d is out of schedule order", i)
		}
	}
	if first.ClockRate != 90000 || first.Interval != 2*time.Millisecond {
		t.Fatalf("clock=%d interval=%s", first.ClockRate, first.Interval)
	}
}

func TestParseImpairment(t *testing.T) {
	impair, err := ParseImpairment("loss=0.01,dup=0.01,reorder=0.02")
	if err != nil {
		t.Fatal(err)
	}
	if impair.Loss != .01 || impair.Duplicate != .01 || impair.Reorder != .02 {
		t.Fatalf("impairment = %+v", impair)
	}
	if _, err := ParseImpairment("loss=1"); err == nil {
		t.Fatal("expected probability range error")
	}
	if _, err := ParseImpairment("unknown=0.1"); err == nil {
		t.Fatal("expected unknown key error")
	}
}

func TestGenerateRejectsShortDuration(t *testing.T) {
	_, err := Generate(Options{Packets: 100, Duration: time.Nanosecond})
	if err == nil {
		t.Fatal("expected interval error")
	}
}
