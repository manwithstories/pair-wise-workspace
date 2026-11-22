package pipeline

import (
	"fmt"

	"mediascan/internal/index"
	"mediascan/internal/media"
	"mediascan/internal/segment"
	"mediascan/internal/stream"
	"mediascan/internal/timeline"
)

// trackEvents converts a track's samples into timeline events. Each event
// represents one sample (for audio) or a run of video samples up to the next
// keyframe boundary, carrying the byte span of its first and last sample so a
// damaged region can be located.
func trackEvents(mp4 []byte, tr *stream.Track, ix index.Index) []timeline.Event {
	if tr == nil || len(tr.Samples) == 0 {
		return nil
	}
	keySet := map[int]bool{}
	for _, n := range ix.Synced {
		keySet[n] = true
	}
	ref := fmt.Sprintf("track-%d", tr.TrackID)
	var out []timeline.Event
	for _, s := range tr.Samples {
		classes, recovery := sampleFaults(tr, s, mp4)
		ev := timeline.Event{
			Kind:         timeline.KindMedia,
			TrackID:      tr.TrackID,
			StartNs:      s.DTSNs,
			EndNs:        s.EndNs,
			IssueClasses: classes,
			Recovery:     recovery,
			Source: segment.Source{
				Kind: tr.Kind, Ref: ref, ByteOffset: s.Offset,
			},
		}
		// A sample that is not a keyframe inherits the recovery state of the
		// keyframe that governs it, so damage stays localized to the GOP.
		if !keySet[s.Number] && recovery == segment.Clean {
			if kf, ok := governingFault(tr, ix, s.Number, mp4); ok {
				ev.IssueClasses = kf.classes
				ev.Recovery = segment.Recovered
			}
		}
		out = append(out, ev)
	}
	return out
}

// sampleFaults reports whether this sample's declared byte span is usable.
func sampleFaults(tr *stream.Track, s stream.Sample, mp4 []byte) ([]string, segment.Recovery) {
	if s.Size <= 0 {
		return []string{string(media.ClassMP4CorruptSample)}, segment.Recovered
	}
	if s.Offset < 0 || s.Offset+s.Size > int64(len(mp4)) {
		return []string{string(media.ClassMP4CorruptSample)}, segment.Recovered
	}
	return nil, segment.Clean
}

// governingFault finds the nearest preceding keyframe whose bytes are damaged
// and returns its fault classes, so a damaged keyframe taints its GOP without
// failing unaffected GOPs.
func governingFault(tr *stream.Track, ix index.Index, sampleNum int, mp4 []byte) (struct{ classes []string }, bool) {
	best := -1
	for _, k := range ix.Synced {
		if k <= sampleNum && k > best {
			best = k
		}
	}
	if best < 0 {
		return struct{ classes []string }{}, false
	}
	for _, s := range tr.Samples {
		if s.Number != best {
			continue
		}
		classes, rec := sampleFaults(tr, s, mp4)
		if rec != segment.Clean {
			return struct{ classes []string }{classes: classes}, true
		}
	}
	return struct{ classes []string }{}, false
}
