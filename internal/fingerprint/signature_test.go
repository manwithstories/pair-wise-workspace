package fingerprint

import "testing"

func samples(seed int) []float64 {
	out := make([]float64, 48_000)
	for i := range out {
		// Different seed changes both waveform and quantized windows.
		out[i] = 0.2 * float64((i/37+seed)%7-3) / 3
	}
	return out
}

func frames(seed byte) []FrameSample {
	return []FrameSample{
		{PTSSec: 0, Data: []byte{seed, 1, 2, 3}},
		{PTSSec: 1, Data: []byte{seed, 4, 5, 6}},
		{PTSSec: 2, Data: []byte{seed, 7, 8, 9}},
	}
}

func TestTwoStageDuplicateRequiresReview(t *testing.T) {
	a := Build("a", samples(1), frames(7), "sha-A", 1, DefaultOptions())
	b := Build("b", samples(1), frames(7), "sha-A", 1, DefaultOptions())
	c := Build("c", samples(9), frames(9), "sha-C", 1, DefaultOptions())
	matches, confirmed, err := DetectDuplicates([]Signature{a, b, c}, DefaultOptions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed != 1 || len(matches) != 1 || !matches[0].Verified {
		t.Fatalf("matches=%+v confirmed=%d", matches, confirmed)
	}
}

func TestQuickHashCollisionWithoutReviewIsNotDuplicate(t *testing.T) {
	a := Build("a", samples(1), frames(7), "full-a", 1, DefaultOptions())
	b := Build("b", samples(1), frames(7), "full-b", 1, DefaultOptions())
	// Simulate a single whole-signature hash collision while preserving the
	// independent window/frame screen.
	b.ReviewSHA256 = "different-full-review"
	matches, confirmed, err := DetectDuplicates([]Signature{a, b}, DefaultOptions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected candidate for review, got %+v", matches)
	}
	if confirmed != 0 || matches[0].Verified {
		t.Fatalf("collision must not confirm duplicate: %+v", matches)
	}
}
