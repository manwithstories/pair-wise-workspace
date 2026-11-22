package worker

import (
	"fmt"
	"sort"

	"mediad/internal/audio"
	"mediad/internal/fault"
	segmenthash "mediad/internal/hash"
	"mediad/internal/index"
)

func crossSegmentFaults(outs []Output) []fault.Fault {
	var fs []fault.Fault
	var prevEnd int64
	havePrev := false
	for _, o := range outs {
		if !o.Determined || o.Gap {
			havePrev = false
			continue
		}
		if havePrev && o.StartTicks != prevEnd {
			fs = append(fs, fault.MustNew(fault.KindTimestampDiscontinuity,
				fault.Location{Segment: o.URI, Offset: 0},
				fmt.Sprintf("segment start %d != previous end %d", o.StartTicks, prevEnd), true, true))
		}
		prevEnd = o.EndTicks
		havePrev = true
	}
	return fs
}

func lookupOutput(inputs []SegmentInput, o Output) (int, bool) {
	for i := range inputs {
		if inputs[i].Segment.URI == o.URI && inputs[i].Segment.Sequence == o.Sequence {
			return i, true
		}
	}
	return 0, false
}

func process(in SegmentInput, target audio.Target, chunk int) Output {
	out := Output{
		URI: in.Segment.URI, Sequence: in.Segment.Sequence, Chunk: chunk,
		StartTicks: in.Segment.Start, EndTicks: in.Segment.End(),
		Determined: in.Segment.DurationKnown, InputHash: in.InputHash,
		InputKnown: in.InputHash != "", Faults: append([]fault.Fault(nil), in.ProbeFaults...),
	}
	if in.Media != nil {
		out.Determined = in.Media.Determined
		if in.Media.Determined {
			out.StartTicks = in.Media.StartTicks
			out.EndTicks = in.Media.EndTicks
		}
	}
	for _, e := range index.Build(in.Segment.URI, in.Segment.Sequence, in.Segment.RepresentationID, in.Media) {
		if e.Determined {
			out.Keyframes = append(out.Keyframes, e)
		}
	}
	if in.Audio != nil {
		audioRes, fl := audio.Normalize(*in.Audio, target, in.Segment.Sequence, in.Segment.RepresentationID)
		out.Faults = append(out.Faults, fl.All()...)
		out.AudioResult = &audioRes
		out.Gap = audioRes.Gap
		if !audioRes.Gap {
			pcm := audio.EncodePCM16(audioRes.Samples)
			out.OutputHash = segmenthash.Bytes(pcm)
			out.OutputSize = int64(len(pcm))
			out.OutputKnown = true
		} else {
			out.Determined = out.Determined && audioRes.DurationTicks > 0
			// A gap emits no samples; its digest is the digest of an empty
			// output, recorded so a resumed run reports the same gap and
			// hash statistics instead of dropping the segment.
			out.OutputHash = segmenthash.Bytes(nil)
			out.OutputSize = 0
			out.OutputKnown = true
		}
	} else {
		out.OutputHash = in.InputHash
		out.OutputSize = in.InputSize
		out.OutputKnown = in.InputHash != ""
	}
	return out
}

func sortOutputs(outs []Output) {
	sort.SliceStable(outs, func(i, j int) bool {
		if outs[i].Sequence != outs[j].Sequence {
			return outs[i].Sequence < outs[j].Sequence
		}
		return outs[i].URI < outs[j].URI
	})
}

func buildIndex(outs []Output) *index.Index {
	idx := &index.Index{}
	for _, o := range outs {
		idx.Entries = append(idx.Entries, o.Keyframes...)
	}
	idx.Entries = index.Merge(idx.Entries)
	idx.Incomplete = false
	for _, o := range outs {
		if !o.Determined || o.Gap {
			idx.Incomplete = true
		}
	}
	return idx
}

// CheckpointFromResult writes one chunk record per completed chunk. When
// prior is supplied its segments for chunks not in res are retained, so a
// second run cannot drop previously completed chunks.
