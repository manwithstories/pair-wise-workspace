package inspect

// Stats summarises a scanned stream.
type Stats struct {
	Tracks         []uint8
	Frames         int
	IDRs           int
	AACFrames      int
	DurationTicks  uint64
	TrackFrames    map[uint8]int
	TrackCodec     map[uint8]string
	TrackIDRs      map[uint8]int
	KeyIntervalMin map[uint8]uint64
	KeyIntervalMax map[uint8]uint64
	KeyIntervalAvg map[uint8]uint64
	Wraps          int
}

// Summarise aggregates scanner state into reportable statistics.
func (s *Scanner) Summarise() Stats {
	st := Stats{
		TrackFrames:    map[uint8]int{},
		TrackCodec:     map[uint8]string{},
		TrackIDRs:      map[uint8]int{},
		KeyIntervalMin: map[uint8]uint64{},
		KeyIntervalMax: map[uint8]uint64{},
		KeyIntervalAvg: map[uint8]uint64{},
		Wraps:          s.WrapCount(),
	}
	st.Tracks = s.Tracks()
	for _, t := range st.Tracks {
		if v, ok := s.lastAbs[t]; ok {
			if d := v - s.firstAbs[t]; d > st.DurationTicks {
				st.DurationTicks = d
			}
		}
	}
	for _, f := range s.frames {
		st.Frames++
		st.TrackFrames[f.Track]++
		switch f.Type {
		case FrameIDR:
			st.IDRs++
			st.TrackIDRs[f.Track]++
			st.TrackCodec[f.Track] = CodecH264
		case FrameNonIDR:
			st.TrackCodec[f.Track] = CodecH264
		case FrameAAC:
			st.AACFrames++
			st.TrackCodec[f.Track] = CodecAAC
		default:
			if _, ok := st.TrackCodec[f.Track]; !ok {
				st.TrackCodec[f.Track] = CodecRaw
			}
		}
	}
	for t, iv := range s.keyIntervals {
		st.KeyIntervalAvg[t] = s.MeanKeyInterval(t)
		if len(iv) == 0 {
			continue
		}
		mn, mx := iv[0], iv[0]
		for _, v := range iv {
			if v < mn {
				mn = v
			}
			if v > mx {
				mx = v
			}
		}
		st.KeyIntervalMin[t] = mn
		st.KeyIntervalMax[t] = mx
	}
	return st
}
