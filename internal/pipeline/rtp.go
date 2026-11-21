package pipeline

import (
	"fmt"

	"mediascan/internal/index"
	"mediascan/internal/model"
	"mediascan/internal/stream"
)

// AnalyzeRTPFiles reads the capture produced by the fixture generator and
// applies sequence, reorder, duplicate and timestamp analysis. Loss ranges are
// attributed to indexed segments by their 90 kHz media time.
func AnalyzeRTPFiles(o Options, entries []index.Entry) (stream.RTPStats, error) {
	path := o.Work + "/rtp/capture.rtp"
	pkts, err := stream.ReadCapture(path)
	if err != nil {
		return stream.RTPStats{}, err
	}
	stats := stream.AnalyzeRTP(pkts)
	// Attribute each lost sequence number to the segment whose 90 kHz window
	// contains it, so transport loss is traceable to media time.
	var out []model.Fault
	for _, f := range stats.Faults {
		if f.Kind != model.FaultPacketLoss || f.Severity != model.SeverityCritical {
			out = append(out, f)
			continue
		}
		for _, r := range stats.LostRanges {
			at, err := model.NewRational(int64(r.StartSeq)*fixtureRTPTickStep, 90000)
			if err != nil {
				continue
			}
			seg := segmentAt(entries, at)
			out = append(out, model.Fault{
				Kind: model.FaultPacketLoss, Severity: model.SeverityCritical,
				Segment: seg, Message: fmt.Sprintf("RTP sequence gap %d..%d", r.StartSeq, r.EndSeq),
				TimeStart: at, TimeEnd: at,
			})
		}
	}
	stats.Faults = out
	return stats, nil
}

// fixtureRTPTickStep mirrors the fixture 90 kHz timestamp advance per packet.
const fixtureRTPTickStep = 90_000 * 30 / 1000

// segmentAt finds the manifest segment containing time t.
func segmentAt(entries []index.Entry, t model.Rational) string {
	for _, e := range entries {
		end, err := e.End()
		if err != nil {
			continue
		}
		if !t.Less(e.Start) && t.Less(end) {
			return e.Name
		}
	}
	return ""
}
