package adapter

import (
	"sort"

	"mediad/internal/media"
)

// RTPV1 turns recovered RTP packet streams into unified samples. Version 1
// treats each RTP timestamp group (one access unit) as a sample; packet
// payloads are concatenated in sequence order without alteration.
type RTPV1 struct{}

// Kind implements Adapter.
func (*RTPV1) Kind() Container { return ContainerRTP }

// Version implements Adapter.
func (*RTPV1) Version() int { return 1 }

// ParseInit implements Adapter. RTP input has no container init segment; the
// caller supplies an InitSegment synthesized from the manifest codec.
func (*RTPV1) ParseInit(data []byte, pos media.SourcePos) (*InitSegment, error) {
	return nil, media.New(media.CodeUnsupported, "rtp-init", pos,
		"RTP input requires an externally supplied track description")
}

// ParseSegment implements Adapter.
func (*RTPV1) ParseSegment(req SegmentRequest) ([]media.Sample, error) {
	if req.RTP == nil || len(req.RTP.Packets) == 0 {
		return nil, media.New(media.CodeSegmentParse, "rtp-segment", req.Pos, "no RTP packets")
	}
	if len(req.Init.Tracks) == 0 {
		return nil, media.New(media.CodeNoTrack, "rtp-segment", req.Pos, "RTP segment has no track description")
	}
	track := req.Init.Tracks[0]
	key, ok := req.Keys[track.ID]
	if !ok {
		return nil, media.New(media.CodeNoTrack, "rtp-segment", req.Pos, "RTP track %d is not mapped", track.ID)
	}
	lim := req.Limits
	if lim == (media.Limits{}) {
		lim = media.DefaultLimits()
	}

	// Group packets into access units: a new AU begins whenever the RTP
	// timestamp changes. Packets within an AU are ordered by sequence.
	type au struct {
		ts      int64
		packets []int
	}
	var groups []*au
	byTS := make(map[int64]*au)
	var refTS int64 = -1
	for i, p := range req.RTP.Packets {
		if p == nil {
			continue
		}
		ts := media.Unwrap32Timestamp(refTS, p.Timestamp)
		if refTS < 0 {
			refTS = ts
		} else if ts > refTS {
			refTS = ts
		}
		g, ok := byTS[ts]
		if !ok {
			g = &au{ts: ts}
			byTS[ts] = g
			groups = append(groups, g)
		}
		g.packets = append(g.packets, i)
	}
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].ts < groups[j].ts })

	samples := make([]media.Sample, 0, len(groups))
	prevTS := int64(0)
	for gi, g := range groups {
		idx := append([]int(nil), g.packets...)
		sort.SliceStable(idx, func(a, b int) bool {
			return req.RTP.Packets[idx[a]].SequenceNumber < req.RTP.Packets[idx[b]].SequenceNumber
		})
		var payload []byte
		marker := false
		for _, i := range idx {
			p := req.RTP.Packets[i]
			payload = append(payload, p.Payload...)
			if p.Marker {
				marker = true
			}
		}
		if len(payload) == 0 {
			continue
		}
		if len(payload) > lim.MaxPacketBytes*64 && lim.MaxPacketBytes > 0 {
			return nil, media.New(media.CodeLimit, "rtp-segment", req.Pos, "access unit exceeds limit")
		}
		dur := int64(3000)
		if gi+1 < len(groups) {
			dur = groups[gi+1].ts - g.ts
		} else if gi > 0 {
			dur = g.ts - prevTS
		}
		if dur <= 0 {
			dur = 1
		}
		prevTS = g.ts
		samples = append(samples, media.Sample{
			Key:      key,
			DTS:      g.ts,
			PTS:      g.ts,
			Duration: dur,
			Sync:     marker || gi == 0,
			Data:     payload,
			Source: media.SourceRef{
				Track:   key.String(),
				Segment: req.SegmentIndex,
				Sample:  gi,
				URI:     req.URI,
				Pos:     req.Pos,
			},
		})
	}
	if len(samples) == 0 {
		return nil, media.New(media.CodeSegmentParse, "rtp-segment", req.Pos, "no access units assembled")
	}
	return samples, nil
}
