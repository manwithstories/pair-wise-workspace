package audio

import "testing"

// TestFlushDrainsFilterDelay proves the resampler's internal filter delay is
// fully emitted at flush: total output matches the rate ratio exactly.
func TestFlushDrainsFilterDelay(t *testing.T) {
	c, _ := NewConverter(DefaultSource(), DefaultTarget())
	if _, err := c.Convert(sineS16(t, 320, 16000, 440)); err != nil {
		t.Fatal(err)
	}
	if c.swr.Delay(int64(c.dst.SampleRate)) == 0 {
		t.Fatal("expected nonzero resampler delay before flush")
	}
	tail, err := c.Flush()
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	in, out := c.Totals()
	if out != c.ExpectedFrames() || in != 320 {
		t.Fatalf("flush did not drain: in=%d out=%d expected=%d tail=%d", in, out, c.ExpectedFrames(), len(tail)/2)
	}
}
