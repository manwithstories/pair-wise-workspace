package fixture

// videoTrak writes the video trak: B-frame ctts, optional edit list,
// optional 32-bit wrap origin, optional stss.
func (g *gen) videoTrak(w *boxWriter) {
	gap, mediaDur := g.videoPresentation()
	w.container("trak", func(w *boxWriter) {
		g.tkhd(w, 1, gap+mediaDur, 1280, 720)
		if g.p.EditList || g.p.TimestampWrap {
			g.edts(w, gap, mediaDur)
		}
		w.container("mdia", func(w *boxWriter) {
			mdhdBox(w, VideoTimescale, uint32(int64(g.p.VideoFrames)*VideoDelta))
			hdlrBox(w, "vide", "VideoHandler")
			w.container("minf", func(w *boxWriter) {
				w.fullBox("vmhd", 0, 1, concat(u16(0), u16(0), u16(0), u16(0)))
				dinfBox(w)
				w.container("stbl", func(w *boxWriter) {
					g.videoStsd(w)
					// stts: uniform frame delta.
					w.fullBox("stts", 0, 0, concat(u32(1), u32(uint32(g.p.VideoFrames)), u32(VideoDelta)))
					g.ctts(w)
					stscBox(w, VideoPerChunk)
					stszBox(w, g.vSizes)
					stcoBox(w, g.vOff)
					if g.p.Inject != "no-stss" {
						g.stss(w)
					}
				})
			})
		})
	})
}

// edts writes the edit list. Wrap origins require elst version 1
// (64-bit media_time); bad-elst injects a truncated entry list.
func (g *gen) edts(w *boxWriter, gap, mediaDur int64) {
	w.container("edts", func(w *boxWriter) {
		mediaTime := g.videoMediaTime()
		rate := concat(u16(1), u16(0))
		if g.p.Inject == "bad-elst" {
			// Declares two entries, carries one: structural corruption.
			entry := concat(u32(uint32(mediaDur)), u32(uint32(int32(mediaTime))), rate)
			w.fullBox("elst", 0, 0, concat(u32(2), entry))
			return
		}
		if g.p.TimestampWrap {
			var entries []byte
			count := uint32(1)
			if g.p.EditList {
				entries = concat(u64(uint64(gap)), u64(0xFFFFFFFFFFFFFFFF), rate) // empty
				count = 2
			}
			entries = append(entries, concat(u64(uint64(mediaDur)), u64(uint64(mediaTime)), rate)...)
			w.fullBox("elst", 1, 0, concat(u32(count), entries))
			return
		}
		var entries []byte
		count := uint32(1)
		if g.p.EditList {
			entries = concat(u32(uint32(gap)), u32(0xFFFFFFFF), rate) // empty segment
			count = 2
		}
		entries = append(entries, concat(u32(uint32(mediaDur)), u32(uint32(int32(mediaTime))), rate)...)
		w.fullBox("elst", 0, 0, concat(u32(count), entries))
	})
}

// ctts writes version-1 composition offsets with a B-frame reorder
// pattern [+1, -1, 0] frames — covers negative offsets.
func (g *gen) ctts(w *boxWriter) {
	pattern := []int32{1, -1, 0}
	payload := u32(uint32(g.p.VideoFrames))
	for i := 0; i < g.p.VideoFrames; i++ {
		payload = append(payload, concat(u32(1), u32(uint32(pattern[i%3]*VideoDelta)))...)
	}
	w.fullBox("ctts", 1, 0, payload)
}

// stss writes the sync-sample table (1-based, every KeyInterval frames).
func (g *gen) stss(w *boxWriter) {
	var entries []byte
	var count uint32
	for i := 0; i < g.p.VideoFrames; i += KeyInterval {
		entries = append(entries, u32(uint32(i+1))...)
		count++
	}
	w.fullBox("stss", 0, 0, concat(u32(count), entries))
}

// videoStsd writes a minimal avc1 sample entry.
func (g *gen) videoStsd(w *boxWriter) {
	entry := concat(
		make([]byte, 6), u16(1), // reserved + data_reference_index
		make([]byte, 16), // pre_defined / reserved
		u16(1280), u16(720),
		u32(0x00480000), u32(0x00480000), // 72 dpi
		u32(0), u16(1), // reserved + frame_count
		make([]byte, 32),       // compressor name
		u16(0x18), u16(0xFFFF), // depth + pre_defined
	)
	sub := &boxWriter{}
	sub.box("avc1", entry)
	w.fullBox("stsd", 0, 0, concat(u32(1), sub.buf.Bytes()))
}

// audioTrak writes a simple AAC-style audio track.
func (g *gen) audioTrak(w *boxWriter) {
	durMs := int64(g.p.AudioFrames) * AudioDelta * MovieTimescale / AudioTimescale
	w.container("trak", func(w *boxWriter) {
		g.tkhd(w, 2, durMs, 0, 0)
		w.container("mdia", func(w *boxWriter) {
			mdhdBox(w, AudioTimescale, uint32(int64(g.p.AudioFrames)*AudioDelta))
			hdlrBox(w, "soun", "SoundHandler")
			w.container("minf", func(w *boxWriter) {
				w.fullBox("smhd", 0, 0, concat(u16(0), u16(0)))
				dinfBox(w)
				w.container("stbl", func(w *boxWriter) {
					g.audioStsd(w)
					w.fullBox("stts", 0, 0, concat(u32(1), u32(uint32(g.p.AudioFrames)), u32(AudioDelta)))
					stscBox(w, AudioPerChunk)
					stszBox(w, g.aSizes)
					stcoBox(w, g.aOff)
				})
			})
		})
	})
}

// audioStsd writes a minimal mp4a sample entry.
func (g *gen) audioStsd(w *boxWriter) {
	entry := concat(
		make([]byte, 6), u16(1),
		make([]byte, 8),
		u16(2), u16(16), // channels + sample size
		u16(0), u16(0),
		u32(AudioTimescale<<16),
	)
	sub := &boxWriter{}
	sub.box("mp4a", entry)
	w.fullBox("stsd", 0, 0, concat(u32(1), sub.buf.Bytes()))
}
