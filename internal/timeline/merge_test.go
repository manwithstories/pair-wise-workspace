package timeline

import "testing"

func ms(v int64) *int64 { return &v }

var msScale = NanosPerTick(1_000_000) // 1 tick = 1 ms

func TestScaleIntegerConversion(t *testing.T) {
	s := NanosPerTick(1_000_000)
	if got := s.ToNanos(1500); got != Nanos(1_500_000_000) {
		t.Fatalf("ToNanos=%d", got)
	}
	hz := Scale{Num: int64(Second), Den: 90000}
	if got := hz.ToNanos(90000 * 12); got != 12*Second {
		t.Fatalf("90kHz conversion=%d", got)
	}
}

func TestMergeInterleavedStableOrder(t *testing.T) {
	m := NewMerger(msScale)
	// Interleave three tracks at the same start time.
	m.Add(1, ms(0), 500, 100)
	m.Add(2, ms(0), 250, 10)
	m.Add(3, ms(0), 1000, 5)
	m.Add(2, ms(250), 250, 10)
	m.Add(1, ms(500), 500, 100)

	ev := m.Events()
	if len(ev) != 5 {
		t.Fatalf("events=%d", len(ev))
	}
	// Same start: ordered by track, then ingestion order.
	if ev[0].Track != 1 || ev[1].Track != 2 || ev[2].Track != 3 {
		t.Fatalf("order at t=0: %d %d %d", ev[0].Track, ev[1].Track, ev[2].Track)
	}
	if ev[3].Track != 2 || ev[3].Start != 250*Millisecond {
		t.Fatalf("event 3: track %d start %d", ev[3].Track, ev[3].Start)
	}
	if ev[4].Track != 1 || ev[4].Start != 500*Millisecond {
		t.Fatalf("event 4: track %d start %d", ev[4].Track, ev[4].Start)
	}
	if len(m.Errors()) != 0 {
		t.Fatalf("unexpected errors: %+v", m.Errors())
	}
	if m.Extent() != 1000*Millisecond {
		t.Fatalf("extent=%d", m.Extent())
	}
}

func TestMissingTimestampInferredFromSameTrack(t *testing.T) {
	m := NewMerger(msScale)
	m.Add(1, ms(0), 500, 100)
	m.Add(1, nil, 500, 100) // missing: infer start = 0 + 500
	m.Add(2, ms(0), 250, 10)
	m.Add(1, nil, 500, 100) // missing: infer start = 500 + 500

	ev := m.Events()
	if len(ev) != 4 {
		t.Fatalf("events=%d", len(ev))
	}
	var inferred []Event
	for _, e := range ev {
		if e.Src == SourceInferred {
			inferred = append(inferred, e)
		}
	}
	if len(inferred) != 2 {
		t.Fatalf("inferred=%d", len(inferred))
	}
	if inferred[0].Start != 500*Millisecond || inferred[1].Start != 1000*Millisecond {
		t.Fatalf("inferred starts: %d %d", inferred[0].Start, inferred[1].Start)
	}
	if len(m.Errors()) != 0 {
		t.Fatalf("unexpected errors: %+v", m.Errors())
	}
}

func TestMissingTimestampWithoutPreviousIsError(t *testing.T) {
	m := NewMerger(msScale)
	m.Add(4, nil, 100, 1)
	if len(m.Events()) != 0 {
		t.Fatalf("events=%d", len(m.Events()))
	}
	errs := m.Errors()
	if len(errs) != 1 || errs[0].Track != 4 {
		t.Fatalf("errors=%+v", errs)
	}
}

func TestBackwardsTimestampIsolatedNotReordered(t *testing.T) {
	m := NewMerger(msScale)
	m.Add(1, ms(1000), 500, 100)
	m.Add(1, ms(500), 500, 100)  // backwards: isolate
	m.Add(1, ms(1500), 500, 100) // continues from the last accepted block

	ev := m.Events()
	if len(ev) != 2 {
		t.Fatalf("events=%d", len(ev))
	}
	if ev[0].Start != 1000*Millisecond || ev[1].Start != 1500*Millisecond {
		t.Fatalf("starts: %d %d", ev[0].Start, ev[1].Start)
	}
	errs := m.Errors()
	if len(errs) != 1 {
		t.Fatalf("errors=%+v", errs)
	}
	if errs[0].Track != 1 || errs[0].Start != 500*Millisecond || errs[0].End != 1000*Millisecond {
		t.Fatalf("error interval: %+v", errs[0])
	}
}
