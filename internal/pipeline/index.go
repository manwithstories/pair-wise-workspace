// Package pipeline indexes keyframes, cuts playable segments, regenerates
// PSI, caches products by content digest and checkpoints resumable work.
package pipeline

import (
	"sort"

	"tsqc/internal/clock"
	"tsqc/internal/ts"
)

// Keyframe is one indexed IDR access unit.
type Keyframe struct {
	PID         uint16
	PTS         int64
	DTS         int64
	PacketIndex int
	ByteOffset  int64
	Segment     int
}

// Index is the keyframe index built from demuxed access units. Byte offsets
// are relative to the start of the recovered transport stream.
type Index struct {
	Keyframes []Keyframe
	VideoPID  uint16
	AudioPID  uint16
	PCRPID    uint16
	Program   uint16
	VideoAUs  int
	AudioAUs  int
}

// BuildIndex scans access units in arrival order and records every IDR.
func BuildIndex(aus []ts.AccessUnit, pmt *ts.PMT) *Index {
	idx := &Index{}
	if pmt != nil {
		idx.Program = pmt.Program
		idx.PCRPID = pmt.PCRPID
		for _, st := range pmt.Streams {
			switch st.Type {
			case ts.StreamH264:
				idx.VideoPID = st.PID
			case ts.StreamAAC:
				idx.AudioPID = st.PID
			}
		}
	}
	for i := range aus {
		au := &aus[i]
		switch au.Kind {
		case ts.KindVideo:
			idx.VideoAUs++
			if au.Keyframe {
				idx.Keyframes = append(idx.Keyframes, Keyframe{
					PID:         au.PID,
					PTS:         au.PTS,
					DTS:         au.DTS,
					PacketIndex: au.FirstPacket,
					ByteOffset:  int64(au.FirstPacket) * ts.PacketSize,
				})
			}
		case ts.KindAudio:
			idx.AudioAUs++
		}
	}
	for i := range idx.Keyframes {
		idx.Keyframes[i].Segment = i
	}
	return idx
}

// KeyframeAtOrAfter returns the first keyframe whose PTS is >= pts.
func (ix *Index) KeyframeAtOrAfter(pts int64) (Keyframe, bool) {
	if pts < 0 {
		return Keyframe{}, false
	}
	i := sort.Search(len(ix.Keyframes), func(i int) bool { return ix.Keyframes[i].PTS >= pts })
	if i >= len(ix.Keyframes) {
		return Keyframe{}, false
	}
	return ix.Keyframes[i], true
}

// GapInterval records a stretch that could not be made playable. Resumption
// always happens at a valid PSI table or IDR, never mid-frame.
type GapInterval struct {
	StartPTS  int64
	EndPTS    int64
	ResumePTS int64
	ResumeIDR bool
	PID       uint16
	Reason    string
}

// TimestampCheck walks every access unit and verifies forward progress per
// stream as well as audio/video sync within the configured tolerance.
type TimestampCheck struct {
	VideoSeen        bool
	AudioSeen        bool
	LastVideo        int64
	LastAudio        int64
	MaxSkew          int64
	VideoRegressions int
	AudioRegressions int
	SkewErrors       int
	SyncError        error
}

// Feed consumes one access unit.
func (t *TimestampCheck) Feed(au ts.AccessUnit) error {
	if !au.HasPTS {
		return nil
	}
	switch au.Kind {
	case ts.KindVideo:
		if t.VideoSeen && au.PTS < t.LastVideo {
			t.VideoRegressions++
			return clock.FatalError(clock.CodeTimestampRegression, "pipeline.CheckTimestamps", "video PTS %d < %d", au.PTS, t.LastVideo)
		}
		t.VideoSeen = true
		t.LastVideo = au.PTS
		if t.AudioSeen {
			d := t.LastVideo - t.LastAudio
			if d < 0 {
				d = -d
			}
			if d > t.MaxSkew {
				t.MaxSkew = d
			}
			if d > clock.SyncToleranceTicks {
				t.SkewErrors++
				if t.SyncError == nil {
					t.SyncError = clock.FatalError(clock.CodeSyncDrift, "pipeline.CheckTimestamps", "A/V skew %d ticks", d)
				}
			}
		}
	case ts.KindAudio:
		if t.AudioSeen && au.PTS < t.LastAudio {
			t.AudioRegressions++
			return clock.FatalError(clock.CodeTimestampRegression, "pipeline.CheckTimestamps", "audio PTS %d < %d", au.PTS, t.LastAudio)
		}
		t.AudioSeen = true
		t.LastAudio = au.PTS
	}
	return nil
}
