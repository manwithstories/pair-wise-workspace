// Package syncer aligns repaired audio and video packet streams onto the
// manifest's shared nanosecond timeline. Both tracks are made monotonic,
// each anchored to its segment origin, and the residual cross-track skew is
// measured between nearest neighbours. Any residual beyond the 20 ms archive
// tolerance is reported rather than hidden.
package syncer

import (
	"sort"

	"archive/internal/manifest"
	"archive/internal/model"
)

// MaxSyncError is the largest tolerated residual timeline deviation after
// alignment. The acceptance contract requires at most 20 milliseconds.
const MaxSyncError = 20 * model.Millisecond

// TrackKind distinguishes the two aligned media tracks.
type TrackKind string

const (
	TrackVideo TrackKind = "video"
	TrackAudio TrackKind = "audio"
)

// AlignedPacket is one packet placed on the shared timeline. ManifestNano
// is the corrected timestamp; NanoTS keeps the pre-correction clock value
// so reports can quantify the correction that was applied.
type AlignedPacket struct {
	Track        TrackKind
	ExtSeq       int64
	NanoTS       model.Nano // clock-derived timestamp before correction
	ManifestNano model.Nano
	SegmentSeq   int
	Retrans      bool
	Recovered    bool
}

// Aligner consumes per-track packets plus the canonical manifest timeline.
// Each track gets one offset mapping its clock origin onto manifest time;
// both tracks therefore share a single monotonic nanosecond coordinate.
type Aligner struct {
	tl         *manifest.Timeline
	offsets    map[TrackKind]model.Nano
	anchorHints map[TrackKind]model.Nano
	anchored   map[TrackKind]bool
	lastTS     map[TrackKind]model.Nano
	maxErr     model.Nano
	log        *model.FaultLog
}

// NewAligner builds an aligner against the given canonical timeline.
func NewAligner(tl *manifest.Timeline, log *model.FaultLog) *Aligner {
	return &Aligner{
		tl:          tl,
		offsets:     map[TrackKind]model.Nano{},
		anchorHints: map[TrackKind]model.Nano{},
		anchored:    map[TrackKind]bool{},
		lastTS:      map[TrackKind]model.Nano{},
		log:         log,
	}
}

// AnchorHint records a known clock offset for a track: when the first
// packet arrives, the given RTP-derived timestamp maps to manifestNano
// instead of the segment origin. It models a capture-clock skew measured
// out of band and lets callers inject a deterministic skew in tests.
func (a *Aligner) AnchorHint(kind TrackKind, rtpNano, manifestNano model.Nano) {
	a.anchorHints[kind] = manifestNano - rtpNano
}

// Align maps one packet to the manifest timeline. The first packet of a
// track anchors its offset to the origin of the containing segment; later
// packets are translated by the same offset. Monotonicity violations are
// clamped forward by one nanosecond and recorded so ordering is preserved
// without silently rewriting history.
func (a *Aligner) Align(kind TrackKind, extSeq int64, nanoTS model.Nano,
	retrans, recovered bool) AlignedPacket {
	if !a.anchored[kind] {
		if hint, ok := a.anchorHints[kind]; ok {
			a.offsets[kind] = hint
		} else {
			origin := model.Nano(0)
			if seg := a.tl.SegmentAt(nanoTS); seg != nil {
				origin = seg.Start
			}
			a.offsets[kind] = origin - nanoTS
		}
		a.anchored[kind] = true
		a.lastTS[kind] = nanoTS + a.offsets[kind] - 1
	}

	manifestNano := nanoTS + a.offsets[kind]
	if manifestNano <= a.lastTS[kind] {
		clamped := a.lastTS[kind] + 1
		a.log.Add(model.Fault{Phase: model.PhaseSync, Kind: model.FaultReorder,
			SeqLo: extSeq, SeqHi: extSeq,
			Detail: "non-monotonic timestamp clamped to preserve ordering"})
		manifestNano = clamped
	}
	a.lastTS[kind] = manifestNano

	segSeq := -1
	if seg := a.tl.SegmentAt(manifestNano); seg != nil {
		segSeq = seg.Seq
	}
	return AlignedPacket{
		Track: kind, ExtSeq: extSeq, NanoTS: nanoTS, ManifestNano: manifestNano,
		SegmentSeq: segSeq, Retrans: retrans, Recovered: recovered,
	}
}

// Finalize sorts the combined output, enforces per-track monotonicity and
// measures the residual A/V skew between each audio packet and its nearest
// video neighbour. Manifest discontinuities are preserved as explicit
// faults. A residual above MaxSyncError is returned as an invariant error.
func (a *Aligner) Finalize(packets []AlignedPacket) ([]AlignedPacket, error) {
	sort.SliceStable(packets, func(i, j int) bool {
		if packets[i].ManifestNano != packets[j].ManifestNano {
			return packets[i].ManifestNano < packets[j].ManifestNano
		}
		if packets[i].Track != packets[j].Track {
			return packets[i].Track == TrackVideo
		}
		return packets[i].ExtSeq < packets[j].ExtSeq
	})
	last := map[TrackKind]model.Nano{}
	var videoTS []model.Nano
	for _, p := range packets {
		if prev, ok := last[p.Track]; ok && p.ManifestNano < prev {
			return nil, model.InvariantError(model.PhaseSync,
				"global order violates monotonicity for %s at seq %d", p.Track, p.ExtSeq)
		}
		last[p.Track] = p.ManifestNano
		if p.Track == TrackVideo {
			videoTS = append(videoTS, p.ManifestNano)
		}
	}
	for _, p := range packets {
		if p.Track != TrackAudio || len(videoTS) == 0 {
			continue
		}
		if d := nearestDistance(p.ManifestNano, videoTS); d > a.maxErr {
			a.maxErr = d
		}
	}
	for _, seg := range a.tl.Segments {
		if seg.Discontinuity {
			a.log.Add(model.Fault{Phase: model.PhaseSync, Kind: model.FaultDiscontinuity,
				Segment: seg.URI, SeqLo: int64(seg.Seq), SeqHi: int64(seg.Seq),
				Detail: "manifest discontinuity preserved on aligned timeline"})
		}
	}
	if a.maxErr > MaxSyncError {
		return packets, model.InvariantError(model.PhaseSync,
			"residual A/V skew %.2f ms exceeds %.0f ms",
			a.maxErr.Milliseconds(), MaxSyncError.Milliseconds())
	}
	return packets, nil
}

// MaxError reports the largest residual nearest-neighbour A/V deviation.
func (a *Aligner) MaxError() model.Nano { return a.maxErr }

// nearestDistance returns the smallest absolute distance of t to any value
// in the sorted video timestamp slice.
func nearestDistance(t model.Nano, sorted []model.Nano) model.Nano {
	i := sort.Search(len(sorted), func(i int) bool { return sorted[i] >= t })
	best := model.Nano(1<<62)
	if i < len(sorted) {
		best = absNano(sorted[i] - t)
	}
	if i > 0 {
		if d := absNano(t - sorted[i-1]); d < best {
			best = d
		}
	}
	return best
}

func absNano(n model.Nano) model.Nano {
	if n < 0 {
		return -n
	}
	return n
}
