package testdata

// buildTrack generates the clean (pre-loss, pre-jitter) RTP datagrams for one
// track and then applies loss and arrival reordering deterministically.
func buildTrack(seed int64, spec TrackSpec, keyframeEvery int) []Datagram {
	clean := make([]Datagram, 0, spec.Packets)
	for i := 0; i < spec.Packets; i++ {
		idx := uint64(i)
		seq := uint16(uint64(spec.SeqStart) + idx)
		var payload []byte
		var pt uint8
		var ts uint32
		var marker bool
		switch spec.Kind {
		case TrackAudio:
			pt = 96
			ts = uint32(uint64(i) * 480) // 10 ms at 48 kHz
			payload = audioPayload(seed, idx)
		default: // TrackVideo
			pt = 97
			ts = uint32(uint64(i) * 3600) // 40 ms at 90 kHz
			keyframe := keyframeEvery > 0 && i%keyframeEvery == 0
			marker = keyframe
			payload = videoPayload(seed, idx, keyframe)
		}
		raw := marshalRTP(spec.SSRC, seq, ts, pt, marker, payload)
		clean = append(clean, Datagram{
			Track:     spec.SSRC,
			Raw:       raw,
			SeqIndex:  idx,
			Timestamp: ts,
		})
	}
	// Loss: deterministic pseudo-random decisions from a per-track stream.
	kept := make([]Datagram, 0, len(clean))
	for i, d := range clean {
		if spec.LossRate > 0 && mix01(uint64(seed)^0x5A5A, uint64(i), uint64(spec.SSRC)) < spec.LossRate {
			continue
		}
		kept = append(kept, d)
	}
	// Jitter: swap N well-separated adjacent pairs without touching content.
	for s := 0; s < spec.JitterSwaps; s++ {
		if len(kept) < 2 {
			break
		}
		pos := int(hashMix(uint64(seed)^0xBEEF, uint64(s)*31+uint64(spec.SSRC)) % uint64(len(kept)-1))
		kept[pos], kept[pos+1] = kept[pos+1], kept[pos]
	}
	return kept
}

// mix01 returns a deterministic value in [0, 1) from three inputs.
func mix01(seed, idx, salt uint64) float64 {
	return float64(hashMix(seed+idx, salt)%1_000_000) / 1_000_000
}

// applyCorruptions mutates the track's datagram list to embed declared faults.
// Truncation and bad-version faults rewrite the datagram in place; duplicate
// faults insert a conflicting extra datagram; bad-RTCP faults are handled by
// the top-level builder because they are control-plane messages.
func applyCorruptions(list *[]Datagram, corruptions []Corruption, track uint32, seed int64) {
	var extras []Datagram
	out := (*list)[:0]
	for _, d := range *list {
		var c *Corruption
		for i := range corruptions {
			if corruptions[i].Track == track && corruptions[i].SeqIndex == d.SeqIndex {
				c = &corruptions[i]
				break
			}
		}
		if c == nil {
			out = append(out, d)
			continue
		}
		switch c.Kind {
		case CorruptTruncated:
			// Cut below the 12-byte RTP fixed header so the truncation is
			// always located by the parser, not mistaken for a short payload.
			cut := 8
			if len(d.Raw) < cut {
				cut = len(d.Raw) / 2
			}
			d.Raw = append([]byte(nil), d.Raw[:cut]...)
			d.HasCorrupt, d.Corrupt = true, c.Kind
		case CorruptBadVersion:
			d.Raw = append([]byte(nil), d.Raw...)
			d.Raw[0] &^= 0xC0 // clear the two version bits
			d.HasCorrupt, d.Corrupt = true, c.Kind
		case CorruptDuplicateConflict:
			dup := Datagram{
				Track:      d.Track,
				Raw:        append([]byte(nil), d.Raw...),
				SeqIndex:   d.SeqIndex,
				Timestamp:  d.Timestamp,
				HasCorrupt: true,
				Corrupt:    c.Kind,
			}
			// Flip a payload byte to force a payload conflict.
			dup.Raw[len(dup.Raw)-1] ^= 0xFF
			extras = append(extras, dup)
		case CorruptBadRTCP:
			// handled in Generate; leave the media packet intact
		}
		out = append(out, d)
	}
	*list = append(out, extras...)
}

// goodRTCP builds a valid minimal receiver report datagram.
func goodRTCP(ssrc uint32) []byte {
	return []byte{0x80, 201, 0x00, 0x01, byte(ssrc >> 24), byte(ssrc >> 16), byte(ssrc >> 8), byte(ssrc)}
}

// badRTCP builds an RTCP datagram whose length word exceeds its actual size.
func badRTCP() []byte {
	return []byte{0x80, 200, 0x00, 0x09, 0, 0, 0, 0}
}

// Generate builds the complete fixture described by spec, interleaving audio
// and video by arrival order, assigning byte offsets and arrival indices, and
// embedding corruptions plus one valid and one invalid RTCP datagram.
func Generate(spec Spec) *Fixture {
	audioSpec := TrackSpec{
		SSRC: spec.AudioSSRC, Kind: TrackAudio, ClockRate: 48000,
		Packets: spec.AudioPkts, SeqStart: spec.AudioSeqStart,
		LossRate: spec.LossRate, JitterSwaps: spec.JitterSwaps,
	}
	videoSpec := TrackSpec{
		SSRC: spec.VideoSSRC, Kind: TrackVideo, ClockRate: 90000,
		Packets: spec.VideoPkts, SeqStart: spec.VideoSeqStart,
		LossRate: spec.LossRate, JitterSwaps: spec.JitterSwaps,
	}
	audio := buildTrack(spec.Seed, audioSpec, spec.KeyframeEvery)
	video := buildTrack(spec.Seed+1, videoSpec, spec.KeyframeEvery)
	applyCorruptions(&audio, spec.Corruptions, spec.AudioSSRC, spec.Seed)
	applyCorruptions(&video, spec.Corruptions, spec.VideoSSRC, spec.Seed)

	fix := &Fixture{AudioSpec: audioSpec, VideoSpec: videoSpec}
	fix.CleanAudio, fix.CleanVideo = countClean(audio), countClean(video)

	var all []Datagram
	// Interleave four audio packets per video packet, matching the 10 ms vs
	// 40 ms timing; leftover audio packets follow.
	ai, vi := 0, 0
	for ai < len(audio) || vi < len(video) {
		for k := 0; k < 4 && ai < len(audio); k++ {
			all = append(all, audio[ai])
			ai++
		}
		if vi < len(video) {
			all = append(all, video[vi])
			vi++
		}
	}
	// Control datagrams: one valid RTCP near the start and, when requested,
	// one invalid RTCP that must never pollute the media timeline.
	all = append(all, Datagram{Track: 0, IsRTCP: true, Raw: goodRTCP(spec.AudioSSRC)})
	for _, c := range spec.Corruptions {
		if c.Kind == CorruptBadRTCP {
			all = append(all, Datagram{
				Track: 0, IsRTCP: true, Raw: badRTCP(),
				HasCorrupt: true, Corrupt: CorruptBadRTCP,
			})
		}
	}

	var offset int64
	for i := range all {
		all[i].Offset = offset
		all[i].Arrival = int64(i)
		offset += int64(len(all[i].Raw))
	}
	fix.Datagrams, fix.TotalOffset = all, offset
	return fix
}

// withSeqStart returns spec with the same wrap-edge sequence start on both
// tracks so the boundary fixture exercises 16-bit sequence wraparound.
func withSeqStart(spec Spec, start uint16) Spec {
	spec.AudioSeqStart, spec.VideoSeqStart = start, start
	return spec
}

// countClean counts datagrams that are not marked as corrupted.
func countClean(list []Datagram) int {
	n := 0
	for _, d := range list {
		if !d.HasCorrupt {
			n++
		}
	}
	return n
}

// MediaDatagrams returns the number of non-RTCP datagrams the fixture offers,
// i.e. the denominator for end-to-end parse/delivery consistency.
func (f *Fixture) MediaDatagrams() int {
	n := 0
	for _, d := range f.Datagrams {
		if !d.IsRTCP {
			n++
		}
	}
	return n
}

// BoundarySpecs returns the named edge-case fixtures required by the spec:
// sequence wrap, empty stream, slow-consumer timeout and no-keyframe video.
func BoundarySpecs(seed int64) map[string]Spec {
	base := DefaultSpec(seed)
	wrap := base
	wrap.AudioPkts = 8
	wrap.VideoPkts = 4
	wrap.AudioSSRC = 0xA11000F0
	wrap.VideoSSRC = 0x71DE00F0
	wrapSeqStart := uint16(0x10000 - 4) // sequences 0xFFFC..0x0003 wrap the edge
	wrap = withSeqStart(wrap, wrapSeqStart)
	noKey := base
	noKey.KeyframeEvery = 0
	noKey.AudioPkts = 8
	noKey.VideoPkts = 4
	empty := base
	empty.AudioPkts = 0
	empty.VideoPkts = 0
	return map[string]Spec{
		"sequence_wrap": wrap,
		"no_keyframe":   noKey,
		"empty_stream":  empty,
		"slow_consumer": base, // timeout behavior is exercised at pipeline level
	}
}
