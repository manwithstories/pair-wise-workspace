package keyschedule

import "testing"

func TestRotationCountBoundaries(t *testing.T) {
	mk := make([]byte, 32)
	s, err := New(mk, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[int]int{0: 0, 1: 1, 4: 1, 5: 2, 16: 4, 17: 5}
	for segments, want := range cases {
		if got := s.RotationCount(segments); got != want {
			t.Errorf("RotationCount(%d)=%d want %d", segments, got, want)
		}
	}
	if s.KeyID(0) != "k-0001" || s.KeyID(15) != "k-0004" {
		t.Fatalf("unexpected key ids %s %s", s.KeyID(0), s.KeyID(15))
	}
	// 墙钟触发：epoch-seconds=2，每 2 虚拟秒一次，与每 4 段取较大 epoch
	st, _ := New(mk, 4, 2)
	if st.EpochFor(3) != 1 || st.EpochFor(4) != 2 {
		t.Fatalf("time-based epoch wrong: %d %d", st.EpochFor(3), st.EpochFor(4))
	}
	// 相同参数派生稳定密钥
	if string(s.KeyFor(7)) != string(s.KeyFor(7)) {
		t.Fatal("key derivation not stable")
	}
}
