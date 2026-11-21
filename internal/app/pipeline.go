package app

import (
	"fmt"
	"io"
	"os"
	"strings"

	"mediameld/internal/ingest"
	"mediameld/internal/manifest"
	"mediameld/internal/mapping"
	"mediameld/internal/model"
	"mediameld/internal/remux"
	"mediameld/internal/timeline"
)

// resolveBundle binds the three delivery roles to concrete tracks using
// content fingerprints. Studio video is the main video, studio audio the main
// audio, and the camera boom channel the auxiliary audio.
func resolveBundle(all []trackBundle) ([]mapping.Resolution, error) {
	var camVideo, studioAudio, camAudio *trackBundle
	for i := range all {
		b := &all[i]
		switch {
		case b.track.Kind == model.KindAudio && strings.Contains(b.input, "studio"):
			if studioAudio == nil {
				studioAudio = b
			}
		case b.track.Kind == model.KindAudio && camAudio == nil:
			camAudio = b
		case b.track.Kind == model.KindVideo && strings.Contains(b.input, "camera") && camVideo == nil:
			camVideo = b
		}
	}
	if camVideo == nil || studioAudio == nil || camAudio == nil {
		return nil, model.NewError(model.CodeTrackMissing,
			"fixture bundle lacks a required role (video=%v mainaudio=%v auxaudio=%v)",
			camVideo != nil, studioAudio != nil, camAudio != nil)
	}
	entries := []mapping.Entry{
		{Role: mapping.RoleMainVideo, Fingerprint: camVideo.track.Fingerprint(), Input: camVideo.input},
		{Role: mapping.RoleMainAudio, Fingerprint: studioAudio.track.Fingerprint(), Input: studioAudio.input},
		{Role: mapping.RoleAuxAudio, Fingerprint: camAudio.track.Fingerprint(), Input: camAudio.input},
	}
	// Every track from each input must be present in the candidate set, or
	// fingerprint lookups for one track would collide with its sibling.
	byInput := map[string][]*model.Track{}
	for _, b := range all {
		byInput[b.input] = append(byInput[b.input], b.track)
	}
	cands := mapping.BuildCandidates(byInput)
	req := mapping.Requirement{
		Video: []mapping.Role{mapping.RoleMainVideo},
		Audio: []mapping.Role{mapping.RoleMainAudio, mapping.RoleAuxAudio},
	}
	return mapping.Resolve(entries, cands, mapping.Policy{}, req)
}

// calibrate turns track samples into unified-timeline events. Each source is
// calibrated with its own rational clock; VFR camera frames use exact integer
// cadence rather than accumulated float rounding.
func calibrate(resolved []mapping.Resolution, opts Options) []model.Event {
	var events []model.Event
	for _, r := range resolved {
		t := r.Track
		// Container timestamps are the canonical acquisition timeline and
		// carry no clock drift; drift and wrap are transport-clock properties
		// unwound on the RTP path.
		clock := model.Clock{Timescale: t.Timescale}
		for i, s := range t.Samples {
			events = append(events, model.Event{
				TrackID: t.ID, Fingerprint: r.Fingerprint, Kind: t.Kind,
				Index: s.Index, Seq: uint32(i + 1),
				DTS: clock.Calibrate(s.DTS), PTS: clock.Calibrate(s.PTS),
				Duration: clock.Calibrate(s.Duration),
				Hash:     s.Hash, Size: s.Size, SourceOffset: s.SourceOffset,
			})
		}
	}
	return events
}

// rtpEvents replaces the camera video timeline with one derived from the
// transport stream: timestamps come from unwrapped, drift-compensated RTP
// ticks, while identity (hash, size, byte offset) and durations come from the
// MXF sample so the remux still copies container bytes verbatim.
func rtpEvents(acq acquisition, cam *mapping.Resolution, containerNS []model.Event) []model.Event {
	var out []model.Event
	byIndex := map[int]model.Event{}
	for _, e := range containerNS {
		if e.TrackID == cam.Track.ID {
			byIndex[e.Index] = e
		} else {
			out = append(out, e)
		}
	}
	var prevEnd model.Instant
	nsByFrame := acq.NS()
	maxIdx := len(nsByFrame)
	if len(byIndex) > 0 {
		for idx := range byIndex {
			if idx+1 > maxIdx {
				maxIdx = idx + 1
			}
		}
	}
	for idx := 0; idx < maxIdx; idx++ {
		ns, ok := acq.byFrame[idx]
		if !ok {
			continue
		}
		base, hasContainer := byIndex[idx]
		ev := model.Event{
			TrackID: cam.Track.ID, Fingerprint: cam.Fingerprint, Kind: model.KindVideo,
			Index: idx, Seq: uint32(idx + 1),
			PTS: model.Instant(ns), DTS: model.Instant(ns),
		}
		if hasContainer {
			ev.Duration = base.Duration
			ev.Hash = base.Hash
			ev.Size = base.Size
			ev.SourceOffset = base.SourceOffset
		} else if prevEnd > 0 {
			ev.Duration = model.Instant(ns) - prevEnd
		}
		if ev.Duration < 0 {
			ev.Duration = 0
		}
		prevEnd = ev.PTS + ev.Duration
		out = append(out, ev)
	}
	return out
}

// buildRemuxInput maps the stable timeline back onto physical source bytes.
func buildRemuxInput(resolved []mapping.Resolution, plan timeline.Plan) remux.Input {
	var in remux.Input
	byID := map[string]mapping.Resolution{}
	for _, r := range resolved {
		byID[r.Track.ID] = r
	}
	// Preserve deterministic track order.
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sortStrings(ids)
	for _, id := range ids {
		r := byID[id]
		// The delivery container normalizes every track to a nanosecond
		// timescale. That makes the stored ticks identical to the unified
		// timeline instants, so no rounding can accumulate.
		ti := remux.TrackInput{
			ID: r.Track.ID, Kind: r.Track.Kind, Timescale: remux.DeliveryTimescale,
			Desc: r.Track.SampleDesc, Width: r.Track.CodedWidth, Height: r.Track.CodedHeight,
			Rotation: r.Decision.Rotation,
		}
		// Consume only non-gap events, which are exactly the delivered
		// samples. DTS is written as the running sum of durations (what stts
		// can express) and PTS stays the exact absolute instant, so the
		// composition offset absorbs any truncation in individual durations.
		var dts int64
		for _, e := range plan.Events {
			if e.TrackID != id || e.IsGap {
				continue
			}
			ti.Samples = append(ti.Samples, remux.InputSample{
				SourcePath: r.Track.SourcePath, Offset: e.SourceOffset, Size: e.Size,
				Hash: e.Hash, TrackID: id, TrackKind: r.Track.Kind,
				Timescale: remux.DeliveryTimescale, DTS: dts, PTS: int64(e.PTS),
				Duration: int64(e.Duration),
			})
			dts += int64(e.Duration)
		}
		in.Tracks = append(in.Tracks, ti)
	}
	return in
}

// verifyOutput re-demuxes the written container and checks it against the
// manifest, independently of the writer's in-memory state.
func verifyOutput(outPath string, man manifest.Manifest) (manifest.Report, error) {
	v := manifest.Verifier{Demux: func() ([]model.Event, error) {
		f, err := os.Open(outPath)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		tracks, err := ingest.DemuxMP4(f, outPath)
		if err != nil {
			return nil, err
		}
		var events []model.Event
		for _, t := range tracks {
			// Output sample times are in the track's own timescale; convert
			// them to unified nanoseconds exactly as the writer did.
			clock := model.Clock{Timescale: t.Timescale}
			for _, s := range t.Samples {
				events = append(events, model.Event{
					TrackID: t.ID, Index: s.Index, Seq: uint32(s.Index + 1),
					DTS: clock.Calibrate(s.DTS), PTS: clock.Calibrate(s.PTS),
					Duration: clock.Calibrate(s.Duration), Hash: s.Hash, Size: s.Size,
				})
			}
		}
		return events, nil
	}}
	return v.Verify(man, segmentSpan)
}

// mainVideoID returns the track id bound to the main video role.
func mainVideoID(resolved []mapping.Resolution) string {
	for _, r := range resolved {
		if r.Role == mapping.RoleMainVideo {
			return r.Track.ID
		}
	}
	return ""
}

func sortStrings(v []string) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

var _ = fmt.Sprint
var _ io.Writer
