package timeline

import "testing"

func TestIdentity(t *testing.T) {
	m, err := NewMapper(nil)
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := m.MediaToPres(123); !ok || p != 123 {
		t.Fatalf("identity: %d %v", p, ok)
	}
}

func TestNonZeroStart(t *testing.T) {
	// Media starts at 90000 (1s at 90kHz); presentation starts at 0.
	m, err := NewMapper([]Edit{{MediaStart: 90000, Duration: 270000}})
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := m.MediaToPres(90000); !ok || p != 0 {
		t.Fatalf("start: %d %v", p, ok)
	}
	if p, ok := m.MediaToPres(90000 + 270000 - 1); !ok || p != 270000-1 {
		t.Fatalf("end: %d %v", p, ok)
	}
	if _, ok := m.MediaToPres(89999); ok {
		t.Fatal("before start must be a gap")
	}
	if mt, ok := m.PresToMedia(0); !ok || mt != 90000 {
		t.Fatalf("inverse: %d %v", mt, ok)
	}
}

func TestMultiEditAndGaps(t *testing.T) {
	m, err := NewMapper([]Edit{
		{MediaStart: 0, Duration: 100},
		{MediaStart: 500, Duration: 100}, // media 100..499 skipped
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.Total() != 200 {
		t.Fatalf("total %d", m.Total())
	}
	if p, ok := m.MediaToPres(500); !ok || p != 100 {
		t.Fatalf("second edit: %d %v", p, ok)
	}
	if _, ok := m.MediaToPres(250); ok {
		t.Fatal("gap must not map")
	}
	if mt, ok := m.PresToMedia(150); !ok || mt != 550 {
		t.Fatalf("inverse: %d %v", mt, ok)
	}
	if _, ok := m.PresToMedia(200); ok {
		t.Fatal("past end must not map")
	}
}

func TestInvalidEdits(t *testing.T) {
	if _, err := NewMapper([]Edit{{MediaStart: -1, Duration: 10}}); err == nil {
		t.Fatal("negative start")
	}
	if _, err := NewMapper([]Edit{{MediaStart: 0, Duration: 0}}); err == nil {
		t.Fatal("zero duration")
	}
}
