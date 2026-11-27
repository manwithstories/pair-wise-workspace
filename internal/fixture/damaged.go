package fixture

import (
	"pairwise/ingestcheck/internal/pipeline"
)

// Pair returns the healthy, lossless sample: every fragment intact, every
// datagram delivered in order, and the two tracks offset by 10 ms.
func Pair() pipeline.Group {
	var frags []pipeline.FragmentInput
	for i := 0; i < VideoFragments; i++ {
		frags = append(frags, toInput(buildVideo(uint32(i)+1, VideoTimeline(i), i)))
	}
	for i := 0; i < AudioFragments; i++ {
		frags = append(frags, toInput(buildAudio(uint32(i)+1, AudioTimeline(i), i)))
	}
	return pipeline.Group{Graph: graphOrPanic(), Fragments: frags}
}

// Truncated returns the damaged sample. One video fragment is cut short
// mid-mdat and another has a box whose declared size runs past the buffer.
// Both are isolated while the remaining fragments continue, and the offsets
// point at the first bad byte of each.
func Truncated() pipeline.Group {
	g := Pair()
	video := 0
	for i := range g.Fragments {
		if g.Fragments[i].TrackID != VideoTrackID {
			continue
		}
		video++
		switch video {
		case 3:
			d := g.Fragments[i].Data
			if len(d) > 40 {
				g.Fragments[i].Data = d[:len(d)-40]
			}
		case 4:
			g.Fragments[i].Data = oversizeBox(g.Fragments[i].Data)
		}
	}
	return g
}

// Desync returns the out-of-sync sample: the audio track is shifted by 200 ms
// relative to video. The aligner must flag the pair as unsynchronised rather
// than fabricate corrected samples.
func Desync() pipeline.Group {
	g := Pair()
	const shift = uint64(200 * 48) // 200 ms at 48 kHz
	for i := range g.Fragments {
		if g.Fragments[i].TrackID != AudioTrackID {
			continue
		}
		ord := int(g.Fragments[i].Seq) - 1
		f := buildAudio(g.Fragments[i].Seq, AudioTimeline(ord)+shift, ord)
		g.Fragments[i] = toInput(f)
	}
	return g
}

// oversizeBox sets the top-level box's declared size far beyond the buffer,
// producing a truncated-box report at the last available byte.
func oversizeBox(data []byte) []byte {
	out := append([]byte(nil), data...)
	if len(out) >= 8 {
		b := uint32(len(out)) + 8192
		out[0] = byte(b >> 24)
		out[1] = byte(b >> 16)
		out[2] = byte(b >> 8)
		out[3] = byte(b)
	}
	return out
}

// Regress returns the timeline-regression sample: the fourth audio fragment
// declares a tfdt roughly half a fragment earlier than its predecessor. The
// parser opens a new epoch instead of rewriting the tick.
func Regress() pipeline.Group {
	g := Pair()
	audio := 0
	for i := range g.Fragments {
		if g.Fragments[i].TrackID != AudioTrackID {
			continue
		}
		audio++
		if audio == 4 {
			ord := int(g.Fragments[i].Seq) - 1
			// Step back a full fragment plus the 10 ms offset so the new base
			// is strictly below the previous fragment's base decode time.
			back := uint64(AudioFrames*AudioFrameTick + AudioOffsetTicks)
			prev := AudioTimeline(ord - 1)
			if prev > back {
				base := prev - back
				f := buildAudio(g.Fragments[i].Seq, base, ord)
				g.Fragments[i] = toInput(f)
			}
		}
	}
	return g
}

// Wrap returns the duplicate-and-wraparound sample. The video datagrams are
// renumbered to straddle the 16-bit boundary, the third datagram is replayed
// as a duplicate, and one datagram arrives far below the drain head as a
// late gap.
func Wrap() pipeline.Group {
	g := Pair()
	var target = -1
	var all [][]byte
	for i := range g.Fragments {
		if g.Fragments[i].TrackID == VideoTrackID {
			if target < 0 {
				target = i
			}
			all = append(all, g.Fragments[i].Packets...)
			g.Fragments[i].Packets = nil
		}
	}
	if target < 0 || len(all) == 0 {
		return g
	}
	for i := range all {
		all[i] = repack(all[i], uint16(0xFFE8+i))
	}
	merged := make([][]byte, 0, len(all)+2)
	for _, d := range all {
		merged = append(merged, append([]byte(nil), d...))
	}
	// A duplicate replay of the third datagram.
	merged = append(merged, append([]byte(nil), all[2]...))
	// A late datagram, well outside the window's duplicate horizon.
	merged = append(merged, repack(all[0], uint16(0xFF00)))
	g.Fragments[target].Packets = merged
	return g
}

func repack(datagram []byte, seq uint16) []byte {
	// The RTP sequence number occupies bytes 2..3 of the fixed header.
	out := append([]byte(nil), datagram...)
	out[2] = byte(seq >> 8)
	out[3] = byte(seq)
	return out
}

// Burst returns the cross-track burst-loss sample. A burst removes a third of
// one video fragment's datagrams (a small, recoverable gap) plus a replayed
// duplicate, while the audio track's mfhd sequence jumps by six so a wide
// hole exists that the budget cannot close and is marked explicit.
func Burst() pipeline.Group {
	g := Pair()
	video := 0
	for i := range g.Fragments {
		if g.Fragments[i].TrackID != VideoTrackID {
			continue
		}
		video++
		if video == 2 {
			n := len(g.Fragments[i].Packets)
			if n > 3 {
				// A replay first: the duplicate must be detected by the window.
				g.Fragments[i].Packets = append(g.Fragments[i].Packets, append([]byte(nil), g.Fragments[i].Packets[1]...))
				// Then drop the middle third to model a burst.
				cut := g.Fragments[i].Packets[:n]
				g.Fragments[i].Packets = append(append(cut[:n/3], cut[2*n/3:]...), g.Fragments[i].Packets[n:]...)
				g.Fragments[i].Missing = n / 3
			}
		}
	}
	// Audio hole: renumber fragments 2..6 so the sequence jumps by six right
	// after the first fragment, without removing any present fragment.
	sep := uint32(6)
	for i := range g.Fragments {
		if g.Fragments[i].TrackID != AudioTrackID {
			continue
		}
		ord := int(g.Fragments[i].Seq) - 1
		if ord == 0 {
			continue
		}
		f := buildAudio(g.Fragments[i].Seq+sep, AudioTimeline(ord), ord)
		f.Seq = g.Fragments[i].Seq + sep
		g.Fragments[i] = toInput(f)
	}
	return g
}
