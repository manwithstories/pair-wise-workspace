package codec

import (
	"bytes"
	"math/rand"
	"testing"
)

func TestRangeCoderRoundTrip(t *testing.T) {
	rnd := rand.New(rand.NewSource(7))
	for _, n := range []int{1, 10, 1000, 100000} {
		src := make([]byte, n)
		for i := range src {
			if rnd.Intn(3) == 0 {
				src[i] = byte(rnd.Intn(256))
			}
		}
		comp := rangeCompress(src)
		back, err := rangeDecompress(comp, len(src))
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if !bytes.Equal(src, back) {
			t.Fatalf("n=%d: mismatch", n)
		}
		t.Logf("n=%d compressed=%d", n, len(comp))
	}
}
