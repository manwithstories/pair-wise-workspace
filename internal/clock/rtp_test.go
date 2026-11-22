package clock_test

import (
	"testing"

	"github.com/pion/rtp"

	"mediacheck/internal/clock"
)

// TestRTPHeaderRoundTrip proves the timestamps this package produces fit the
// pion/rtp header layout a transmitter would marshal.
func TestRTPHeaderRoundTrip(t *testing.T) {
	cases := []int64{0, 1, 33, 1000, 3600_000, 1 << 32}
	for _, ms := range cases {
		ts := clock.RTPTimestamp(ms, 90000)
		h := rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: 7, Timestamp: ts, SSRC: 0x1234}
		buf, err := h.Marshal()
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var got rtp.Header
		if _, err := got.Unmarshal(buf); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Timestamp != ts {
			t.Fatalf("ts %d round-trip=%d want %d", ms, got.Timestamp, ts)
		}
	}
}

func TestRTPMonotonicForShortedWindow(t *testing.T) {
	var prev uint32
	for ms := int64(0); ms <= 600000; ms += 1000 {
		ts := clock.RTPTimestamp(ms, 90000)
		if ms > 0 && ts <= prev {
			t.Fatalf("timestamp did not advance at %d ms", ms)
		}
		prev = ts
	}
}
