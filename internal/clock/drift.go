package clock

// DefaultDriftThresholdPPM is the drift magnitude above which a track is
// corrected segment by segment.
const DefaultDriftThresholdPPM = 500.0

type driftSample struct {
	ref   int64 // reference (common timeline) media ticks
	track int64 // track media ticks at the same instant
}

// Correction records one segment-level drift adjustment applied to a
// track, in 90 kHz media ticks.
type Correction struct {
	Track       string
	Segment     int
	PPM         float64
	OffsetTicks int64 // correction applied to the track timeline
}

// DriftEstimator observes (reference, track) media-time pairs for each
// track and derives per-segment drift corrections. Drift at or below the
// threshold is left alone; above it, the segment is corrected and the
// correction is recorded so nothing shifts silently.
type DriftEstimator struct {
	thresholdPPM float64
	segmentLen   int
	tracks       map[string][]driftSample
}

// NewDriftEstimator builds an estimator. segmentLen is the number of
// observation pairs per correction segment.
func NewDriftEstimator(thresholdPPM float64, segmentLen int) *DriftEstimator {
	if segmentLen < 2 {
		segmentLen = 2
	}
	return &DriftEstimator{
		thresholdPPM: thresholdPPM,
		segmentLen:   segmentLen,
		tracks:       make(map[string][]driftSample),
	}
}

// Observe records that a track's media time trackTime corresponds to
// reference media time refTime.
func (d *DriftEstimator) Observe(track string, refTime, trackTime int64) {
	d.tracks[track] = append(d.tracks[track], driftSample{ref: refTime, track: trackTime})
}

// Corrections analyses one track and returns the segment corrections
// required to keep it within the drift threshold of the reference.
func (d *DriftEstimator) Corrections(track string) []Correction {
	samples := d.tracks[track]
	var out []Correction
	for seg, i := 0, 0; i+d.segmentLen <= len(samples); i, seg = i+d.segmentLen, seg+1 {
		chunk := samples[i : i+d.segmentLen]
		first, last := chunk[0], chunk[len(chunk)-1]
		refDelta := last.ref - first.ref
		trackDelta := last.track - first.track
		if refDelta <= 0 {
			continue
		}
		ppm := float64(trackDelta-refDelta) / float64(refDelta) * 1e6
		if ppm > d.thresholdPPM || ppm < -d.thresholdPPM {
			out = append(out, Correction{
				Track:       track,
				Segment:     seg,
				PPM:         ppm,
				OffsetTicks: -(trackDelta - refDelta),
			})
		}
	}
	return out
}
