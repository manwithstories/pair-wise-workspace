package timeline_test

import (
	"strings"
	"testing"

	"mediasync/internal/media"
	"mediasync/internal/testdata"
	"mediasync/internal/timeline"
)

func TestParseTimecodeStreamRolloverAndDuplicates(t *testing.T) {
	for _, fx := range testdata.TimecodeFixtures() {
		rate, err := timeline.ParseRate(fx.Rate)
		if err != nil {
			t.Fatalf("%s: %v", fx.Rate, err)
		}
		input := strings.Join(fx.Labels, "\n") + "\n"
		res := timeline.ParseTimecodeStream([]byte(input), rate)
		if res.Stats.Accepted != len(fx.Labels) {
			t.Fatalf("%s: accepted %d of %d labels: %v", fx.Rate, res.Stats.Accepted, len(fx.Labels), res.Issues)
		}
		if len(res.Issues) != 0 {
			t.Fatalf("%s: unexpected issues: %v", fx.Rate, res.Issues)
		}
		// Unwrapped frame numbers must be strictly increasing except the
		// duplicate record, which repeats its predecessor.
		for i := 1; i < len(res.Records); i++ {
			prev, cur := res.Records[i-1], res.Records[i]
			if cur.Unwrapped < prev.Unwrapped {
				t.Fatalf("%s: record %d went backwards (%d < %d)", fx.Rate, i, cur.Unwrapped, prev.Unwrapped)
			}
			if cur.Unwrapped == prev.Unwrapped && !cur.Duplicate {
				t.Fatalf("%s: repeat at %d not marked duplicate", fx.Rate, i)
			}
		}
		if fx.Rate == "25" {
			if res.Stats.Wrapped != 1 {
				t.Errorf("25fps: wrapped = %d, want 1 day rollover", res.Stats.Wrapped)
			}
			if res.Stats.Duplicate != 1 {
				t.Errorf("25fps: duplicates = %d, want 1", res.Stats.Duplicate)
			}
			if !res.Trimmed() {
				t.Error("25fps: Trimmed() = false after a rollover")
			}
		}
	}
}

func TestParseTimecodeStreamRejectsBrokenLabels(t *testing.T) {
	fx := testdata.TimecodeFixtures()[3] // 24 fps with broken labels
	rate, _ := timeline.ParseRate(fx.Rate)
	input := strings.Join(fx.Labels, "\n") + "\n" + strings.Join(fx.Broken, "\n") + "\n"
	res := timeline.ParseTimecodeStream([]byte(input), rate)
	if res.Stats.Rejected != len(fx.Broken) {
		t.Fatalf("rejected %d, want %d", res.Stats.Rejected, len(fx.Broken))
	}
	if len(res.Issues) != len(fx.Broken) {
		t.Fatalf("got %d issues", len(res.Issues))
	}
	for _, is := range res.Issues {
		if is.Stage != media.StageTimebase || is.Input != media.InputTC {
			t.Errorf("issue metadata = %+v", is)
		}
		if !is.HasOffset {
			t.Errorf("issue without offset: %v", is)
		}
	}
	// Offsets must point at the start of the offending label.
	text := []byte(input)
	for _, rec := range res.Issues {
		line := text[rec.Offset:]
		if idx := strings.IndexByte(string(line), '\n'); idx >= 0 {
			line = line[:idx]
		}
		if !strings.Contains(string(line), ":") {
			t.Errorf("offset %d does not address a timecode label: %q", rec.Offset, line)
		}
	}
	if res.Stats.Accepted != len(fx.Labels) {
		t.Errorf("accepted %d, want %d", res.Stats.Accepted, len(fx.Labels))
	}
}

func TestParseTimecodeStreamEmpty(t *testing.T) {
	res := timeline.ParseTimecodeStream([]byte("\n\n  \n"), timeline.Rate25)
	if res.Stats.Labels != 0 || len(res.Records) != 0 || len(res.Issues) != 0 {
		t.Errorf("empty stream = %+v", res)
	}
}
