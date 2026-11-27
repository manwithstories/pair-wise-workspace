package avsync

import (
	"testing"

	"bcastqc/internal/media"
)

func TestAlignedTracksSmallDiff(t *testing.T) {
	a := New()
	// 视频每 33ms 一帧；音频每 10ms 一块，偏差始终 < 5ms。
	for i := 0; i < 30; i++ {
		vNS := int64(i) * 33_000_000
		if err := a.Add(TimedPoint{Track: media.TrackVideo, TimeNS: vNS, Key: i%10 == 0}); err != nil {
			t.Fatal(err)
		}
		// 为每个视频点附近放 3 个音频点。
		for k := 0; k < 3; k++ {
			aNS := vNS + int64(k-1)*10_000_000 + 2_000_000
			if aNS < 0 {
				continue
			}
			if err := a.Add(TimedPoint{Track: media.TrackAudio, TimeNS: aNS}); err != nil {
				t.Fatal(err)
			}
		}
	}
	rep := a.Compute()
	if rep.Pairs == 0 {
		t.Fatal("no pairs computed")
	}
	if rep.MaxDiffMS() > 5 {
		t.Fatalf("max diff %dms exceeds 5ms", rep.MaxDiffMS())
	}
	if err := VerifyThreshold(rep, 20_000_000); err != nil {
		t.Fatalf("within threshold but flagged: %v", err)
	}
}

func TestBackwardsAndNegativesCountedNotFixed(t *testing.T) {
	a := New()
	_ = a.Add(TimedPoint{Track: media.TrackVideo, TimeNS: 100_000_000})
	_ = a.Add(TimedPoint{Track: media.TrackVideo, TimeNS: 90_000_000}) // 回退
	_ = a.Add(TimedPoint{Track: media.TrackAudio, TimeNS: -5_000_000}) // 负值
	_ = a.Add(TimedPoint{Track: media.TrackAudio, TimeNS: 10_000_000})
	rep := a.Compute()
	if rep.Backwards != 1 || rep.Negatives != 1 {
		t.Fatalf("backwards=%d negatives=%d", rep.Backwards, rep.Negatives)
	}
	// 回退点仍按原值参与，时间线未被改写。
	if a.video[1].TimeNS != 90_000_000 {
		t.Fatal("timestamp was silently modified")
	}
}

func TestThresholdExceeded(t *testing.T) {
	a := New()
	_ = a.Add(TimedPoint{Track: media.TrackVideo, TimeNS: 0})
	_ = a.Add(TimedPoint{Track: media.TrackVideo, TimeNS: 100_000_000})
	_ = a.Add(TimedPoint{Track: media.TrackAudio, TimeNS: 0})
	_ = a.Add(TimedPoint{Track: media.TrackAudio, TimeNS: 150_000_000}) // 偏差 50ms
	rep := a.Compute()
	if err := VerifyThreshold(rep, 20_000_000); err == nil {
		t.Fatal("expected sync threshold error")
	} else if !media.IsKind(err, media.KindSync) {
		t.Fatalf("wrong error kind: %v", err)
	}
}

func TestEmptyTrackNoFakePairs(t *testing.T) {
	a := New()
	_ = a.Add(TimedPoint{Track: media.TrackVideo, TimeNS: 0})
	rep := a.Compute()
	if rep.Pairs != 0 {
		t.Fatal("must not fabricate audio pairs")
	}
}
