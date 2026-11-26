package media_test

import (
	"errors"
	"testing"

	"avpipe/internal/media"
)

func TestClockExpandsPCRWrapWithoutEpochChange(t *testing.T) {
	c := media.NewClock()
	firstRaw := media.PCRMod27 - 27_000_000 // one 90 kHz second before wrap
	tick0, epoch0, err := c.FeedPCR(firstRaw, 100, false)
	if err != nil {
		t.Fatal(err)
	}
	tick1, epoch1, err := c.FeedPCR(0, 288, false)
	if err != nil {
		t.Fatal(err)
	}
	if tick1 <= tick0 {
		t.Fatalf("PCR did not expand monotonically: %d -> %d", tick0, tick1)
	}
	if tick1-tick0 != 90_000 {
		t.Fatalf("delta=%d want 90000", tick1-tick0)
	}
	if epoch1.ID != epoch0.ID || epoch1.ID != 0 {
		t.Fatalf("wrap created epoch change: %+v -> %+v", epoch0, epoch1)
	}
	if c.Epochs[0].PCRWraps != 1 {
		t.Fatalf("wrap count=%d want 1", c.Epochs[0].PCRWraps)
	}
}

func TestClockDiscontinuityStartsEpoch(t *testing.T) {
	c := media.NewClock()
	if _, _, err := c.FeedPCR(1_000_000, 0, false); err != nil {
		t.Fatal(err)
	}
	_, epoch, err := c.FeedPCR(2_000_000, 188, true)
	if err != nil {
		t.Fatal(err)
	}
	if epoch.ID != 1 || len(c.Epochs) != 2 {
		t.Fatalf("epochs=%+v want new epoch", c.Epochs)
	}
}

func TestClockWithoutPCRAnchorIsUnrecoverable(t *testing.T) {
	c := media.NewClock()
	err := c.RequireAnchor()
	if err == nil {
		t.Fatal("missing PCR anchor accepted")
	}
	var me *media.Error
	if !errors.As(err, &me) || me.Code != media.CodeNoPCRAnchor {
		t.Fatalf("error=%v want no_pcr_anchor", err)
	}
	if code := media.ExitCode(err); code != 3 {
		t.Fatalf("exit=%d want 3", code)
	}
}
