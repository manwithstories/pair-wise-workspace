package audio

import "testing"

// TestConverterRejectsBadSpec guards the constructor validation path.
func TestConverterRejectsBadSpec(t *testing.T) {
	if _, err := NewConverter(SourceSpec{SampleRate: 0, Channels: 1}, DefaultTarget()); err == nil {
		t.Fatal("zero source rate must be rejected")
	}
	if _, err := NewConverter(DefaultSource(), TargetSpec{SampleRate: 0, Channels: 2}); err == nil {
		t.Fatal("zero target rate must be rejected")
	}
}

// TestUseAfterCloseRejected ensures a closed converter cannot be reused.
func TestUseAfterCloseRejected(t *testing.T) {
	c, _ := NewConverter(DefaultSource(), DefaultTarget())
	if _, err := c.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Convert(sineS16(t, 16, 16000, 300)); err == nil {
		t.Fatal("convert after flush must fail")
	}
}
