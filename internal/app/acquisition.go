package app

import (
	"os"
	"sort"

	"mediameld/internal/ingest"
	"mediameld/internal/mapping"
	"mediameld/internal/model"
)

// acquisition is the transport-side view of the main video track: calibrated
// nanoseconds per frame index plus receive order.
type acquisition struct {
	byFrame map[int]int64
	order   []int
	ns      []int64 // ns per frame index, -1 for undelivered frames
}

// readAcquisition decodes and calibrates the RTP stream. Calibration here is
// what compensates the 32-bit wrap and the source clock drift; the result is
// used to validate the container-derived camera timeline rather than to
// replace it, so a mismatch surfaces as an error instead of silent drift.
func readAcquisition(path string, driftPPM int64) (acquisition, error) {
	f, err := os.Open(path)
	if err != nil {
		return acquisition{}, model.WrapError(model.CodeInternal, err, "open rtp")
	}
	defer f.Close()
	res, err := ingest.DemuxRTP(f, "cam-video")
	if err != nil {
		return acquisition{}, err
	}
	cal := ingest.CalibrateRTP(res, driftPPM)
	a := acquisition{byFrame: map[int]int64{}, order: cal.Order, ns: cal.NS}
	for i, ns := range cal.NS {
		if ns >= 0 {
			a.byFrame[i] = ns
		}
	}
	if !cal.Monotonic {
		return a, model.NewError(model.CodeClock,
			"RTP timestamps remain non-monotonic after unwrap and drift calibration")
	}
	return a, nil
}

// acquisitionDriftNS reports the maximum |container - transport| difference
// per frame. A non-zero bounded value proves the wrap and drift calibration
// agreed with the container timeline to within one tick.
// NS exposes the calibrated per-frame nanoseconds, with -1 for frames the
// transport never delivered.
func (a acquisition) NS() []int64 { return a.ns }

func (a acquisition) maxDriftNS(events []model.Event, trackID string) int64 {
	var worst int64
	for _, e := range events {
		if e.TrackID != trackID {
			continue
		}
		ns, ok := a.byFrame[e.Index]
		if !ok {
			continue
		}
		diff := int64(e.PTS) - ns
		if diff < 0 {
			diff = -diff
		}
		if diff > worst {
			worst = diff
		}
	}
	return worst
}

// applyArrival stamps the main video track with RTP receive positions so a
// displaced packet is detected as late. Other tracks use their own order.
func applyArrival(events []model.Event, a acquisition, videoTrackID string) {
	pos := make(map[int]int, len(a.order))
	for i, idx := range a.order {
		if _, seen := pos[idx]; !seen {
			pos[idx] = i
		}
	}
	for i := range events {
		if events[i].TrackID != videoTrackID {
			events[i].Arrival = 1_000_000 + events[i].Index
			continue
		}
		if p, ok := pos[events[i].Index]; ok {
			events[i].Arrival = p
		} else {
			// A frame that never arrived is trailing, not silently dropped.
			events[i].Arrival = 2_000_000 + events[i].Index
		}
	}
}

// stableIDs returns track ids in deterministic order.
func stableIDs(res []mapping.Resolution) []string {
	ids := make([]string, 0, len(res))
	for _, r := range res {
		ids = append(ids, r.Track.ID)
	}
	sort.Strings(ids)
	return ids
}

// resolutionFor finds the resolved role bound to a track id.
func resolutionFor(res []mapping.Resolution, id string) *mapping.Resolution {
	for i := range res {
		if res[i].Track.ID == id {
			return &res[i]
		}
	}
	return nil
}
