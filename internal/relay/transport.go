package relay

import (
	"fmt"

	"relay.local/relay/internal/audit"
	"relay.local/relay/internal/fault"
	"relay.local/relay/internal/fixture"
	"relay.local/relay/internal/index"
	"relay.local/relay/internal/metadata"
	"relay.local/relay/internal/model"
	"relay.local/relay/internal/scene"
	"relay.local/relay/internal/timeline"
	"relay.local/relay/internal/transport"
)

type streamResult struct {
	name              string
	expected          int // distinct packets the generator actually sent
	expectedLost      int // packets the generator removed outright
	expectedReordered int // packets the generator delayed out of order
	got               transport.Result
}

// ok reports whether recovery matched the ground truth exactly: every packet
// the generator actually sent appears once, nothing was dropped by the reorder
// window, and the only losses are the packets the generator removed.
func (r streamResult) ok() bool {
	return r.got.Stats.Unique == r.expected &&
		r.got.Stats.Dropped == 0 &&
		r.got.Stats.Lost == r.expectedLost &&
		r.got.Stats.Recovered == r.expectedReordered
}

// recoverStreams recovers every generated stream through the real wire
// encoding and aggregates wrap counts and client-side statistics.
func recoverStreams(sc *scene.Scenario, rep *Report) []streamResult {
	out := make([]streamResult, 0, len(sc.RTP))
	recovered := 0
	for _, s := range sc.RTP {
		res := recoverOne(sc, s)
		sr := streamResult{
			name:              s.Name,
			expected:          s.Stream.Total - s.Stream.Dropped,
			expectedLost:      s.Stream.Dropped,
			expectedReordered: s.Stream.Reordered,
			got:               res,
		}
		out = append(out, sr)
		rep.Wraps += res.Stats.Wraps
		if sr.ok() {
			recovered++
		}
		for _, it := range res.Faults.Items {
			rep.Faults.Add(it)
		}
	}
	rep.Recovered = fmt.Sprintf("%d/%d", recovered, len(sc.RTP))
	return out
}

// recoverOne round-trips one stream through pion/rtp and transport.Recover.
func recoverOne(_ *scene.Scenario, s scene.RTPStream) transport.Result {
	bufs, err := fixture.Marshal(s.Stream.Packets)
	if err != nil {
		res := transport.Result{Faults: &fault.List{}}
		res.Faults.Add(fault.Wrap(-1, 0, fault.KindCorrupt, false, err))
		return res
	}
	wire, err := fixture.Unmarshal(bufs)
	if err != nil {
		res := transport.Result{Faults: &fault.List{}}
		res.Faults.Add(fault.Wrap(-1, 0, fault.KindCorrupt, false, err))
		return res
	}
	parsed := make([]transport.Packet, 0, len(wire))
	for i, p := range wire {
		parsed = append(parsed, transport.Packet{
			Seq: p.SequenceNumber, Timestamp: p.Timestamp, Marker: p.Marker,
			TrackID: scene.RTPTrackID, Payload: p.Payload, Arrival: i + 1,
		})
	}
	return transport.Recover(parsed, transport.Options{
		Window: 8, MaxPackets: 1 << 16, MaxPayload: 1 << 20,
	})
}

// buildIndex builds and validates the keyframe index over video tracks.
func buildIndex(sc *scene.Scenario, rep *Report) {
	total, hits := 0, 0
	for _, clip := range []scene.Clip{sc.MP4, sc.MXF} {
		if clip.File == nil {
			continue
		}
		video := videoTracks(clip.File.Tracks)
		idx := index.Build(video, clip.File.Data)
		total += idx.Count()
		for _, e := range idx.Entries {
			tr, ok := trackByID(clip.File.Tracks, e.TrackID)
			if !ok {
				continue
			}
			if s, ok := tr.SampleByIndex(e.SampleIdx); ok && s.Sync && s.Offset == e.Offset {
				hits++
			}
		}
		rep.VFRIntervals = maxInt(rep.VFRIntervals, distinctDurations(video))
	}
	rep.Keyframe = fmt.Sprintf("%d/%d", hits, total)
}

func videoTracks(tracks []*model.Track) []*model.Track {
	out := make([]*model.Track, 0, len(tracks))
	for _, t := range tracks {
		if t.Kind == model.Video {
			out = append(out, t)
		}
	}
	return out
}

func distinctDurations(tracks []*model.Track) int {
	set := map[int64]bool{}
	for _, t := range tracks {
		for _, s := range t.Samples {
			set[s.Duration] = true
		}
	}
	return len(set)
}

func trackByID(tracks []*model.Track, id uint32) (*model.Track, bool) {
	for _, t := range tracks {
		if t.ID == id {
			return t, true
		}
	}
	return nil, false
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// checkDiagnostics proves the two diagnostic paths the spec requires: an index
// whose entries are corrupted must be invalidated and rebuilt by scanning, and
// a track carrying an unknown colour marker must be isolated while the rest of
// the composition continues.
func checkDiagnostics(sc *scene.Scenario, rep *Report) error {
	// Corrupt-index rebuild: take the MXF video track, build a good index, then
	// poison one entry's offset and confirm Validate rejects it while
	// Revalidate restores a fully valid table.
	video, ok := scene.VideoTrack(sc.MXF)
	if !ok {
		return fault.New(-1, 0, fault.KindCorrupt, false, "MXF clip has no video track")
	}
	idx := index.Build([]*model.Track{video}, sc.MXF.File.Data)
	if !idx.Valid || idx.Count() == 0 {
		return fault.New(-1, 0, fault.KindCorrupt, false, "MXF keyframe index is invalid before corruption")
	}
	good := idx.Count()
	idx.Entries[0].Offset += 3
	if err := idx.Validate([]*model.Track{video}, sc.MXF.File.Data); err == nil {
		return fault.New(-1, 0, fault.KindCorrupt, false,
			"a corrupted keyframe offset was accepted by Validate")
	}
	idx.Revalidate([]*model.Track{video}, sc.MXF.File.Data)
	if !idx.Valid || idx.Rebuilds != 1 || idx.Count() != good {
		return fault.New(-1, 0, fault.KindCorrupt, false,
			"index rebuild failed: valid=%v rebuilds=%d entries=%d/%d",
			idx.Valid, idx.Rebuilds, idx.Count(), good)
	}
	rep.Notes = append(rep.Notes, fmt.Sprintf("keyframe_rebuild=%d/%d", idx.Count(), good))

	// Unknown-colour isolation: attach the marker to a copy of the video track
	// and resolve it. The resolver marks it isolated; the original composition
	// is unaffected because it resolves a different track value.
	probe := &model.Track{
		ID: 999, Kind: model.Video, Codec: model.CodecMJPEG,
		Claims: model.Claims{
			Policy:    map[string]string{},
			Bitstream: map[string]string{},
			Container: map[string]string{"color": "unknown:zzzz"},
		},
	}
	res := metadata.Resolve(probe, metadata.Options{})
	if !res.Isolated || !probe.Isolated {
		return fault.New(-1, 0, fault.KindCorrupt, false,
			"unknown colour marker did not isolate the track")
	}
	isolated := false
	for _, it := range res.Faults.Items {
		if it.Kind == fault.KindMetadata && it.Recoverable {
			isolated = true
		}
	}
	if !isolated {
		return fault.New(-1, 0, fault.KindCorrupt, false,
			"colour isolation produced no recoverable diagnostic")
	}
	rep.Notes = append(rep.Notes, "color_isolation=1")
	return nil
}

// verifyAlignment proves the cross-node path: for each recovered RTP stream,
// drift fitted from sync anchors must stay within the 500 ppm bound, and the
// drift-corrected node time must track the local MP4 frame spacing within a few
// ticks. Streams are fitted independently because each node has its own epoch.
func verifyAlignment(sc *scene.Scenario, recs []streamResult, rep *Report) error {
	mp4Track, ok := scene.VideoTrack(sc.MP4)
	if !ok {
		return fault.New(-1, 0, fault.KindCorrupt, false, "no MP4 video track")
	}
	// Map frame content to its global index; recovery drops some packets, so
	// payload identity is the only stable link back to the source frame.
	indexOf := make(map[string]int, len(sc.Frames))
	for i, f := range sc.Frames {
		indexOf[audit.HashBytes(f)] = i
	}
	for _, sr := range recs {
		anchors := streamAnchors(sr, indexOf, mp4Track)
		if len(anchors) < 2 {
			continue
		}
		fit := timeline.FitDrift(anchors)
		if fit.Unaligned {
			return fault.New(-1, 0, fault.KindDrift, false,
				"%s drift could not be aligned: %s", sr.name, fit.Reason)
		}
		if err := checkSpacing(sr, fit, indexOf, mp4Track); err != nil {
			return err
		}
		rep.Notes = append(rep.Notes, fmt.Sprintf("drift_ppm_%s=%d", sr.name, fit.PPM))
	}
	return nil
}

// streamAnchors pairs each recovered packet of one stream with its local frame
// time, using payload identity to recover the source frame index.
func streamAnchors(sr streamResult, indexOf map[string]int, mp4Track *model.Track) []timeline.Anchor {
	var anchors []timeline.Anchor
	for i := range sr.got.Packets {
		if i >= len(sr.got.Ticks) {
			break
		}
		global, ok := indexOf[audit.HashBytes(sr.got.Packets[i].Payload)]
		if !ok || global >= len(mp4Track.Samples) {
			continue
		}
		local, err := mp4Track.DTS90k(mp4Track.Samples[global])
		if err != nil {
			continue
		}
		anchors = append(anchors, timeline.Anchor{
			Index: len(anchors), NodeTick: sr.got.Ticks[i], Local90k: local,
		})
	}
	return anchors
}

// checkSpacing compares, per stream, the node-clock spacing of consecutive
// recovered packets against the local spacing of the same frames. Drift
// correction must reduce the residual to a few ticks.
func checkSpacing(sr streamResult, fit timeline.DriftFit,
	indexOf map[string]int, mp4Track *model.Track) error {
	var prevLocal, prevNode int64 = -1, -1
	for i := range sr.got.Packets {
		if i >= len(sr.got.Ticks) {
			break
		}
		global, ok := indexOf[audit.HashBytes(sr.got.Packets[i].Payload)]
		if !ok || global >= len(mp4Track.Samples) {
			continue
		}
		local, err := mp4Track.DTS90k(mp4Track.Samples[global])
		if err != nil {
			return fault.Wrap(-1, 0, fault.KindCorrupt, false, err)
		}
		node := sr.got.Ticks[i]
		if prevLocal >= 0 {
			localSpan := local - prevLocal
			nodeSpan := fit.NodeToLocal(node) - fit.NodeToLocal(prevNode)
			if diff := localSpan - nodeSpan; diff < -3 || diff > 3 {
				return fault.New(-1, 0, fault.KindDrift, false,
					"%s spacing mismatch: local %d vs corrected node %d (diff %d)",
					sr.name, localSpan, nodeSpan, diff)
			}
		}
		prevLocal, prevNode = local, node
	}
	return nil
}
