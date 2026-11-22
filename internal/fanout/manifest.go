package fanout

import (
	"fmt"

	"mediacheck/internal/fault"
	"mediacheck/internal/repair"
)

// ManifestInput is the recovery result plus the ladder settings.
type ManifestInput struct {
	Recovery *repair.Result
	Config   Config
}

// FromRecovery fans the recovered video track out into the ladder and fans
// the renditions back into one manifest. Audio is summarized separately so a
// missing video track does not discard usable audio results.
func FromRecovery(in ManifestInput) (*Manifest, error) {
	if in.Recovery == nil {
		return nil, fmt.Errorf("fanout: nil recovery result")
	}
	cfg, err := in.Config.Normalize()
	if err != nil {
		return nil, err
	}
	video := pickVideo(in.Recovery)
	if video == nil {
		return nil, fmt.Errorf("fanout: no usable video track")
	}
	m, err := Build(video, cfg)
	if err != nil {
		return nil, err
	}
	// Record the boundary verdict from the recovery stage so a caller sees
	// both the cross-layer acceptance and the per-rendition alignment.
	if !in.Recovery.Boundary.Accept {
		m.Switchable = false
		m.Reasons = append(m.Reasons, in.Recovery.Boundary.Reasons...)
	}
	m.Issues = append(m.Issues, in.Recovery.Boundary.Issues...)
	if audio := pickAudio(in.Recovery); audio != nil {
		m.Audio = summarizeAudio(audio)
	}
	m.RTP = summarizeRTP(video, RTPClockRate)
	return m, nil
}

// AudioSummary reports the audio layer that shares the manifest's clock.
type AudioSummary struct {
	TrackID    uint32  `json:"track_id"`
	Codec      string  `json:"codec"`
	Samples    int     `json:"valid_samples"`
	Bytes      int64   `json:"bytes"`
	ActualBPS  float64 `json:"actual_bps"`
	DurationMS int64   `json:"duration_ms"`
	StartMS    int64   `json:"start_ms"`
	Complete   bool    `json:"complete"`
}

func summarizeAudio(t *repair.TrackResult) *AudioSummary {
	dur := t.Recovered.EndMS - t.Recovered.StartMS
	return &AudioSummary{
		TrackID:    t.TrackID,
		Codec:      t.Codec,
		Samples:    t.Recovered.Samples,
		Bytes:      t.Recovered.Bytes,
		ActualBPS:  bitsPerSecond(t.Recovered.Bytes, dur),
		DurationMS: dur,
		StartMS:    t.Recovered.StartMS,
		Complete:   t.Recovered.Complete,
	}
}

func pickVideo(r *repair.Result) *repair.TrackResult {
	for _, t := range r.Tracks {
		if t.Kind == "video" && t.Recovered.Samples > 0 {
			return t
		}
	}
	return nil
}

func pickAudio(r *repair.Result) *repair.TrackResult {
	for _, t := range r.Tracks {
		if t.Kind == "audio" && t.Recovered.Samples > 0 {
			return t
		}
	}
	return nil
}

// ReportIssues flattens manifest and recovery faults.
func ReportIssues(m *Manifest, r *repair.Result) []*fault.Fault {
	var out []*fault.Fault
	if r != nil {
		out = append(out, r.Issues...)
	}
	if m != nil {
		out = append(out, m.Issues...)
		out = append(out, m.RTP.Issues...)
	}
	return out
}
