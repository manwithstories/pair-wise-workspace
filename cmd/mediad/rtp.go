package main

import (
	"encoding/json"
	"os"

	"mediad/internal/fixture"
	rtppacket "mediad/internal/rtp"
	"mediad/internal/worker"
)

// RTPSummary is the RTP evidence written next to each case.
type RTPSummary struct {
	Packets   []rtppacket.Evidence `json:"packets"`
	Monotonic bool                 `json:"monotonic"`
}

// RTPSummaryName is the per-case RTP evidence file.
const RTPSummaryName = "rtp_evidence.json"

// writeRTPEvidence maps valid normalised segments onto RTP timestamp and
// audio-level evidence. It returns the summary so the report can mention it.
func writeRTPEvidence(dir string, res *worker.Result, target float64) (RTPSummary, error) {
	summary := RTPSummary{Monotonic: true}
	for _, o := range res.Outputs {
		if o.AudioResult == nil || o.Gap || !o.Determined || !o.OutputKnown {
			continue
		}
		frames := len(o.AudioResult.Samples) / maxInt(o.AudioResult.Channels, 1)
		lufs := o.AudioResult.MeasuredLUFS
		ev, _, err := rtppacket.Build(o.URI, o.Sequence, o.StartTicks, frames, lufs)
		if err != nil {
			return summary, err
		}
		summary.Packets = append(summary.Packets, ev)
	}
	summary.Monotonic = rtppacket.Monotonic(summary.Packets)
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return summary, err
	}
	path, err := fixture.SafeJoin(dir, RTPSummaryName)
	if err != nil {
		return summary, err
	}
	return summary, os.WriteFile(path, data, 0o644)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
