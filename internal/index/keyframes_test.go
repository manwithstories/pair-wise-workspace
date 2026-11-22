package index

import "testing"

func TestFloorAndExact(t *testing.T) {
	ix := New()
	// Insert out of order on purpose.
	ix.Add(90000, 2)
	ix.Add(0, 0)
	ix.Add(45000, 1)
	if ix.Len() != 3 {
		t.Fatalf("len=%d", ix.Len())
	}
	kt, id, ok := ix.Floor(50000)
	if !ok || kt != 45000 || id != 1 {
		t.Fatalf("floor(50000)=%d,%d,%v", kt, id, ok)
	}
	if _, _, ok := ix.Floor(-1); ok {
		t.Fatal("floor(-1) should miss")
	}
	if id, ok := ix.Exact(90000); !ok || id != 2 {
		t.Fatalf("exact(90000)=%d,%v", id, ok)
	}
	if _, ok := ix.Exact(90001); ok {
		t.Fatal("exact(90001) should miss")
	}
	// Sorted access.
	for i := 1; i < ix.Len(); i++ {
		prev, _ := ix.At(i - 1)
		cur, _ := ix.At(i)
		if cur < prev {
			t.Fatal("index not sorted")
		}
	}
}
