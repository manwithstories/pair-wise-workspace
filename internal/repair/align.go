package repair

import (
	"fmt"

	"github.com/pairwise/mediad/internal/ingest"
	"github.com/pairwise/mediad/internal/media"
)

// SegmentInput is one expected media segment and its observed RTP packets.
type SegmentInput struct {
	TrackID       string
	TrackNumber   uint32
	GlobalIndex   int
	SegmentInTrack int
	Timescale     uint32
	DurationUnits uint64
	TFDT          uint64
	Discontinuity bool
	Keyframe      bool
	SeqStart      uint64
	SeqEnd        uint64
	Arrival       []ingest.Packet
	Retransmit    []ingest.Packet
	Payload       []byte
	BandwidthBps  int
}

// TrackInput groups a track's segments.
type TrackInput struct {
	ID          string
	Number      uint32
	Timescale   uint32
	Init        []byte
	Segments    []SegmentInput
}

// BandwidthStep is one bandwidth transition.
type BandwidthStep struct {
	AtGlobalSegment int `json:"at_global_segment"`
	Bps             int `json:"bps"`
}

// AlignedSegment is a repaired, canonical fMP4 segment.
type AlignedSegment struct {
	GlobalIndex   int                  `json:"global_index"`
	TrackID       string               `json:"track_id"`
	SegmentInTrack int                 `json:"segment_in_track"`
	OutputRel     string               `json:"output_rel"`
	StartTicks    int64                `json:"start_ticks"`
	EndTicks      int64                `json:"end_ticks"`
	TFDTOut       uint64               `json:"tfdt_out"`
	Complete      bool                 `json:"complete"`
	Keyframe      bool                 `json:"keyframe"`
	Window        int                  `json:"window"`
	Missing       []uint64             `json:"missing,omitempty"`
	Gaps          []ingest.Gap         `json:"gaps,omitempty"`
	Data          []byte               `json:"-"`
	TimelineEvent *media.TimelineEvent `json:"timeline_event,omitempty"`
}

// Report summarizes alignment and damage evidence.
type Report struct {
	Segments        []AlignedSegment    `json:"segments"`
	Duplicates      int                 `json:"duplicates"`
	Conflicts       []ingest.Conflict   `json:"conflicts,omitempty"`
	BurstLosses     int                 `json:"burst_losses"`
	Recovered       int                 `json:"recovered"`
	BandwidthSteps  int                 `json:"bandwidth_steps"`
	WindowTrace     []int               `json:"window_trace"`
	TimelineEvents  []media.TimelineEvent `json:"timeline_events,omitempty"`
	Incomplete      int                 `json:"incomplete_segments"`
}

// WindowForBandwidth maps a bandwidth tier to a bounded reorder window.
func WindowForBandwidth(bps int) int {
	switch {
	case bps >= 4_000_000:
		return 128
	case bps >= 2_000_000:
		return 64
	case bps >= 1_000_000:
		return 32
	default:
		return 16
	}
}

// WindowFull reports whether enough later sequence space has passed to consider
// a gap expired from a full bounded reorder window.
func WindowFull(gap ingest.Gap, highestObserved uint64, window int) bool {
	if window <= 0 || highestObserved < gap.To {
		return false
	}
	return int(highestObserved-gap.To+1) >= window
}

func missingInRange(missing []uint64, start, end uint64) []uint64 {
	var out []uint64
	for _, seq := range missing {
		if seq >= start && seq <= end {
			out = append(out, seq)
		}
	}
	return out
}

// Align converges RTP packets, applies the common rational clock, repairs
// recoverable gaps, and emits canonical fMP4 segments.
func Align(tracks []TrackInput, steps []BandwidthStep) (*Report, error) {
	report := &Report{BandwidthSteps: len(steps)}
	tl := media.NewTimeline()
	var conflictErr error

	for _, track := range tracks {
		if track.Timescale == 0 {
			return nil, media.NewError(media.CodeMissingField, "repair.align", track.ID+".timescale", "track timescale required")
		}
		if track.Init != nil {
			if err := ValidateBoxes(track.Init); err != nil {
				return nil, err
			}
		}
		stream := ingest.NewStream(64)
		for _, seg := range track.Segments {
			window := WindowForBandwidth(seg.BandwidthBps)
			stream.Window = window
			report.WindowTrace = append(report.WindowTrace, window)

			for _, pkt := range seg.Arrival {
				if err := stream.Accept(pkt); err != nil {
					if me, ok := media.AsError(err); ok && me.Code == media.CodePacketConflict {
						if conflictErr == nil {
							conflictErr = err
						}
						continue
					}
					return nil, err
				}
			}
			before := stream.Flush()
			lost := missingInRange(before.Missing, seg.SeqStart, seg.SeqEnd)
			report.BurstLosses += len(lost)

			for _, pkt := range seg.Retransmit {
				pkt.UnwrappedSeq = media.UnwrapUint(16, seg.SeqStart-1, uint64(pkt.Sequence), true)
				// Retransmitted sequence is near the segment's unwrapped start.
				if pkt.UnwrappedSeq < seg.SeqStart {
					pkt.UnwrappedSeq += 1 << 16
				}
				if err := stream.Retransmit(pkt); err != nil {
					if conflictErr == nil {
						conflictErr = err
					}
				}
			}
			report.Recovered += len(seg.Retransmit)

			final := stream.Flush()
			missing := missingInRange(final.Missing, seg.SeqStart, seg.SeqEnd)
			var gaps []ingest.Gap
			for _, g := range final.Gaps {
				if g.To >= seg.SeqStart && g.From <= seg.SeqEnd {
					gaps = append(gaps, g)
				}
			}

			start, ev := tl.Resolve(track.ID, int64(seg.TFDT), track.Timescale, seg.Discontinuity)
			durationTicks := media.ConvertTicks(
				int64(seg.DurationUnits),
				media.Rational{Num: 1, Den: int64(track.Timescale)},
				media.CommonTimebase,
			)
			if durationTicks <= 0 {
				durationTicks = 180000
			}
			end := start + durationTicks
			tfdtOut := uint64(media.ConvertTicks(start, media.CommonTimebase, media.Rational{Num: 1, Den: int64(track.Timescale)}))

			complete := len(missing) == 0 && len(seg.Payload) > 0
			keyframe := seg.Keyframe && complete
			if !complete {
				report.Incomplete++
			}
			out := AlignedSegment{
				GlobalIndex:    seg.GlobalIndex,
				TrackID:        track.ID,
				SegmentInTrack: seg.SegmentInTrack,
				OutputRel:      fmt.Sprintf("segments/%s-segment-%03d.m4s", track.ID, seg.SegmentInTrack),
				StartTicks:     start,
				EndTicks:       end,
				TFDTOut:        tfdtOut,
				Complete:       complete,
				Keyframe:       keyframe,
				Window:         window,
				Missing:        missing,
				Gaps:           gaps,
				TimelineEvent:  ev,
			}
			if ev != nil {
				report.TimelineEvents = append(report.TimelineEvents, *ev)
			}
			if complete {
				out.Data = BuildFragment(FragmentOptions{
					SequenceNumber: uint32(seg.GlobalIndex + 1),
					TrackID:        track.Number,
					TFDT:           tfdtOut,
					Payload:        seg.Payload,
				})
				if err := ValidateBoxes(out.Data); err != nil {
					return nil, err
				}
			}
			report.Segments = append(report.Segments, out)
		}
		finalReport := stream.Flush()
		report.Duplicates += finalReport.Duplicates
		report.Conflicts = append(report.Conflicts, finalReport.Conflicts...)
	}
	if conflictErr != nil {
		return report, conflictErr
	}
	return report, nil
}
