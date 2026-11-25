package container

import (
	"fmt"

	"archivectl/internal/timeline"
)

// FidelityReport compares demuxed source and remuxed output samples.
type FidelityReport struct {
	TrackCount          int
	SampleCount         int
	PayloadBytes        int64
	PayloadHashMismatch int
	TimestampMismatch   int
	MissingTracks       int
}

// VerifyAssets checks sample hashes, byte counts and raw DTS/PTS values.
func VerifyAssets(source, output *Asset) (FidelityReport, error) {
	var r FidelityReport
	if source == nil || output == nil {
		return r, timeline.New(timeline.CodeIntegrity, "verify assets", "nil asset")
	}
	r.TrackCount = len(source.Tracks)
	for _, st := range source.Tracks {
		ot, ok := output.TrackByID(st.ID)
		if !ok {
			r.MissingTracks++
			continue
		}
		if len(ot.Samples) != len(st.Samples) {
			r.PayloadHashMismatch += absInt(len(ot.Samples) - len(st.Samples))
		}
		n := minInt(len(st.Samples), len(ot.Samples))
		for i := 0; i < n; i++ {
			a := st.Samples[i]
			b := ot.Samples[i]
			r.SampleCount++
			r.PayloadBytes += int64(a.Size)
			if a.Size != b.Size || a.Hash != b.Hash || HashBytes(b.Data) != b.Hash {
				r.PayloadHashMismatch++
			}
			if a.DTS != b.DTS || a.PTS != b.PTS {
				r.TimestampMismatch++
			}
		}
	}
	if r.MissingTracks > 0 || r.PayloadHashMismatch > 0 || r.TimestampMismatch > 0 {
		return r, &timeline.Error{Code: timeline.CodeIntegrity, Op: "verify remux", Offset: -1,
			Detail: fmt.Sprintf("missing_tracks=%d payload_hash_mismatch=%d timestamp_mismatch=%d",
				r.MissingTracks, r.PayloadHashMismatch, r.TimestampMismatch)}
	}
	return r, nil
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
