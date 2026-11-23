package archive

import "testing"

func TestChecksumDeterministic(t *testing.T) {
	v := map[string]any{"b": []int{1, 2, 3}, "a": "x"}
	a, err := Checksum(v)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Checksum(v)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("same value, different checksums")
	}
	if len(a) != 64 {
		t.Fatalf("checksum length = %d, want 64 hex chars", len(a))
	}
	c, _ := Checksum(map[string]any{"a": "y"})
	if c == a {
		t.Fatal("different values, same checksum")
	}
}
