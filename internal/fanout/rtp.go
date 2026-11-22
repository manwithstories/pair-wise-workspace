package fanout

import (
	"github.com/pion/rtp"

	"mediacheck/internal/clock"
	"mediacheck/internal/fault"
	"mediacheck/internal/repair"
)

// RTPClockRate is the video RTP clock used for timestamp metadata.
const RTPClockRate uint32 = 90000

// SampleRTP is one recovered keyframe boundary as an RTP timestamp.
type SampleRTP struct {
	TrackID   uint32 `json:"track_id"`
	Sample    uint64 `json:"sample_index"`
	DTSMS     int64  `json:"dts_ms"`
	Timestamp uint32 `json:"rtp_timestamp"`
	Marker    bool   `json:"marker"`
}

// rtpsummary maps recovered keyframe boundaries onto the 32-bit RTP clock so
// a transmitter can align marker packets to the shared timeline.
// RTPSummary exposes keyframe RTP timestamps for the report and transmitter.
type RTPSummary struct {
	ClockRate  uint32         `json:"clock_rate"`
	Keyframes  []SampleRTP    `json:"keyframe_timestamps"`
	Wrapped    bool           `json:"wrapped"`
	MaxDeltaMS int64          `json:"max_delta_ms"`
	Issues     []*fault.Fault `json:"-"`
}

// summarizeRTP converts each recovered keyframe to an RTP timestamp. The
// header type from pion/rtp is used so downstream packet builders see the
// exact field layout this summary is meant to feed.
func summarizeRTP(t *repair.TrackResult, rate uint32) *RTPSummary {
	out := &RTPSummary{ClockRate: rate}
	if t == nil || len(t.KeyframeTimes) == 0 {
		return out
	}
	var prev uint32
	var prevMS int64
	for i, ms := range t.KeyframeTimes {
		ts := clock.RTPTimestamp(ms, rate)
		var sample uint64
		if i < len(t.KeyframeSamples) {
			sample = t.KeyframeSamples[i]
		}
		out.Keyframes = append(out.Keyframes, SampleRTP{
			TrackID:   t.TrackID,
			Sample:    sample,
			DTSMS:     ms,
			Timestamp: ts,
			Marker:    true,
		})
		if i > 0 {
			delta := clock.RTPDiff(ts, prev)
			if delta < 0 {
				out.Wrapped = true
			}
			if d := clock.RTPDeltaMS(uint32(delta), rate); d > out.MaxDeltaMS {
				out.MaxDeltaMS = d
			}
			// The RTP delta must equal the wall-clock delta within 1 ms; a
			// mismatch means the two layers disagree on the clock.
			want := ms - prevMS
			if got := clock.RTPDeltaMS(uint32(delta), rate); clock.AbsDiff(got, want) > 1 && !out.Wrapped {
				out.Issues = append(out.Issues, fault.ForTrack(fault.CodeTimestamp, fault.SeverityWarning,
					"rtp", t.TrackID, -1,
					"RTP timestamp delta disagrees with the shared clock"))
			}
		}
		prev = ts
		prevMS = ms
		_ = rtp.Header{Version: 2, Timestamp: ts, Marker: true}
	}
	return out
}
