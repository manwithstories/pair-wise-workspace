package fixture

import (
	"bq/internal/manifest"
)

// planInjection fills the manifest's deterministic injection descriptor.
// Packet loss itself is sampled at delivery time by transport from the
// manifest seed, so the loss pattern stays reproducible without being stored.
func planInjection(m *manifest.Manifest, opts Options) {
	inj := manifest.Injection{LossRate: opts.LossRate}

	if opts.InjectWrap {
		for _, s := range m.Streams {
			if s.WrapPeriod > 0 {
				inj.WrappedStreams = append(inj.WrappedStreams, s.ID)
			}
		}
	}
	// One duplicate on the first video stream, placed past the first GOP.
	if opts.InjectDup && len(m.Streams) > 0 {
		vid := m.Streams[0].ID
		if p, ok := nthMedia(m, vid, 35); ok {
			inj.Duplicates = append(inj.Duplicates, manifest.Dup{StreamID: vid, Seq: p.Seq})
		}
	}
	// Tail truncation on the last video stream: cut the final ~4% of packets.
	if opts.InjectTrunc {
		vid := m.Streams[len(m.Streams)-2].ID // last video track
		if pkts := mediaOf(m, vid); len(pkts) > 20 {
			cut := int64(float64(len(pkts)) * 0.96)
			last := pkts[cut-1].Seq
			inj.TruncateAfterSeq = append(inj.TruncateAfterSeq, manifest.Trunc{
				StreamID: vid, AfterSeq: last, DeclaredCount: int64(len(pkts)),
			})
		}
	}
	// Anchor conflicts are assigned deterministically to mid-run anchors.
	if opts.AnchorConflicts > 0 {
		inj.AnchorConflicts = opts.AnchorConflicts
		assignAnchorConflicts(m, opts.AnchorConflicts)
	}
	m.Injection = inj
}

func assignAnchorConflicts(m *manifest.Manifest, count int) {
	assigned := 0
	// Pick anchors at 60% of the run, spread across streams, and corrupt the
	// expected master tick by a clearly impossible margin.
	for _, s := range m.Streams {
		if assigned >= count {
			break
		}
		for i := range m.Anchors {
			a := &m.Anchors[i]
			if a.StreamID != s.ID || a.Conflicts {
				continue
			}
			if a.AtSecond >= m.DurationS*6/10 {
				a.Conflicts = true
				a.MasterTicks += 500 * 90 // +500 ms, far beyond 20 ms tolerance
				assigned++
				break
			}
		}
	}
}

func mediaOf(m *manifest.Manifest, streamID int) []manifest.Packet {
	var out []manifest.Packet
	for _, p := range m.Packets {
		if p.StreamID == streamID && p.Kind == manifest.KindMedia {
			out = append(out, p)
		}
	}
	return out
}

func nthMedia(m *manifest.Manifest, streamID int, n int) (manifest.Packet, bool) {
	pkts := mediaOf(m, streamID)
	if n >= len(pkts) {
		return manifest.Packet{}, false
	}
	return pkts[n], true
}
