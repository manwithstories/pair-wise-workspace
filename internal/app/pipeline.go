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
		clock := model.Clock{Timescale: t.Timescale, DriftPPM: opts.DriftPPM}
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

// rtpArrival is the receive order of camera frames decoded from the RTP file.
type rtpArrival struct {
	order []int
}

// demuxRTP decodes the arrival order so late frames can be flagged even when
// their presentation timestamps are early.
func demuxRTP(path string) (rtpArrival, error) {
	f, err := os.Open(path)
	if err != nil {
		return rtpArrival{}, model.WrapError(model.CodeInternal, err, "open rtp")
	}
	defer f.Close()
	res, err := ingest.DemuxRTP(f, "cam")
	if err != nil {
		return rtpArrival{}, err
	}
	order := make([]int, 0, len(res.Packets))
	seen := map[uint16]bool{}
	for _, p := range res.Packets {
		if seen[p.Seq] {
			continue
		}
		seen[p.Seq] = true
		order = append(order, int(p.Seq)-1000)
	}
	return rtpArrival{order: order}, nil
}

// applyArrival stamps camera video events with their RTP receive position.
// Other tracks have no transport schedule, so their own sample order is their
// arrival order and they are offset into a disjoint range to keep comparisons
// meaningful only within a track.
func applyArrival(events []model.Event, rtp rtpArrival, videoTrackID string) {
	pos := map[int]int{}
	for i, srcIdx := range rtp.order {
		pos[srcIdx] = i
	}
	for i := range events {
		if events[i].TrackID != videoTrackID {
			events[i].Arrival = 1_000_000 + events[i].Index
			continue
		}
		if p, ok := pos[events[i].Index]; ok {
			events[i].Arrival = p
		} else {
			// A frame that never arrived is treated as trailing, not dropped.
			events[i].Arrival = 2_000_000 + events[i].Index
		}
	}
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
		ti := remux.TrackInput{
			ID: r.Track.ID, Kind: r.Track.Kind, Timescale: r.Track.Timescale,
			Desc: r.Track.SampleDesc, Width: r.Track.CodedWidth, Height: r.Track.CodedHeight,
			Rotation: r.Decision.Rotation,
		}
		// Consume only non-gap events, which are exactly the delivered samples.
		for _, e := range plan.Events {
			if e.TrackID != id || e.IsGap {
				continue
			}
			ti.Samples = append(ti.Samples, remux.InputSample{
				SourcePath: r.Track.SourcePath, Offset: e.SourceOffset, Size: e.Size,
				Hash: e.Hash, TrackID: id, TrackKind: r.Track.Kind,
				Timescale: r.Track.Timescale, DTS: int64(e.DTS), PTS: int64(e.PTS),
				Duration: int64(e.Duration),
			})
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
			for _, s := range t.Samples {
				events = append(events, model.Event{
					TrackID: t.ID, Index: s.Index, Seq: uint32(s.Index + 1),
					DTS: model.Instant(s.DTS), PTS: model.Instant(s.PTS),
					Duration: model.Instant(s.Duration), Hash: s.Hash, Size: s.Size,
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
