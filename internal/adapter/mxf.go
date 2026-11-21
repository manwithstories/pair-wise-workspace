package adapter

import (
	"context"
	"encoding/binary"

	"mediacore/internal/media"
)

// MXFAdapter parses the declared MXF subset: a single-partition OP1a file
// with a metadata local set, interleaved essence KLVs and an optional index
// segment. Unknown KLVs are skipped and counted; a KLV whose declared length
// crosses the end of the input is reported at that KLV's offset.
type MXFAdapter struct{}

func (*MXFAdapter) Format() string  { return "mxf" }
func (*MXFAdapter) Version() string { return "1.0.0" }

func (*MXFAdapter) Detect(head []byte, size int64) bool {
	// Detection survives a header truncated inside its partition pack, so
	// only the first eight bytes of the UL are required.
	return len(head) >= 8 && head[0] == 0x06 && head[1] == 0x0e &&
		head[2] == 0x2b && head[3] == 0x34 &&
		head[4] == 0x02 && head[5] == 0x05 && head[6] == 0x01 && head[7] == 0x01
}

type mxfTrackMeta struct {
	number uint32
	rateN  uint32
	rateD  uint32
	audio  bool
	seen   bool
}

type mxfSample struct {
	offset    int64
	size      int64
	track     uint32
	timestamp int64 // ticks at the track rate, expanded if wrapping
}

// Parse walks KLVs sequentially. The walk is bounded by the real file size,
// so truncation is detected before any payload is touched.
func (a *MXFAdapter) Parse(ctx context.Context, src media.Source) (*media.Container, error) {
	if err := ctx.Err(); err != nil {
		return nil, media.NewError(media.ClassCancelled, "cancelled before parse").WithCause(err)
	}
	w := &mxfWalker{src: src, size: src.Size()}
	if err := w.scan(ctx); err != nil {
		return nil, err
	}
	c, err := w.container()
	if err != nil {
		return nil, err
	}
	c.Fingerprint, err = fingerprint(src, c)
	if err != nil {
		return nil, err
	}
	media.SortTracks(c.Tracks)
	return c, nil
}

type mxfWalker struct {
	src        media.Source
	size       int64
	metas      []*mxfTrackMeta
	byTrack    map[uint32]*mxfTrackMeta
	unknown    int
	samples    map[uint32][]mxfSample
	indexTimes map[uint32][]int64
	sawIndex   bool
	sawHeader  bool
	ambigAt    int64
}

func (w *mxfWalker) scan(ctx context.Context) error {
	w.byTrack = make(map[uint32]*mxfTrackMeta, 4)
	w.samples = make(map[uint32][]mxfSample, 4)
	w.indexTimes = make(map[uint32][]int64, 4)
	w.ambigAt = -1
	hdr := make([]byte, 20)
	off := int64(0)
	for off < w.size {
		if err := ctx.Err(); err != nil {
			return media.NewError(media.ClassCancelled, "cancelled while scanning MXF").WithCause(err)
		}
		if w.size-off < 16 {
			return media.NewError(media.ClassTruncated,
				"%d trailing bytes cannot hold a KLV key", w.size-off).
				WithContainer("mxf").WithOffset(w.size).WithTolerance(0)
		}
		// Read a full 16-byte key plus the longest BER length (5 bytes) in
		// one bounded call; a short read is only legal at the very end.
		n, err := w.src.ReadAt(hdr, off)
		if n < 16 {
			return media.NewError(media.ClassTruncated, "KLV key is truncated").
				WithContainer("mxf").WithOffset(off).WithTolerance(0).WithCause(err)
		}
		var key [16]byte
		copy(key[:], hdr[:16])
		if n < 17 {
			return media.NewError(media.ClassTruncated, "KLV length is truncated").
				WithContainer("mxf").WithOffset(off).WithTolerance(0)
		}
		length, lenBytes, ok := berLen(hdr[16:n])
		if !ok {
			return media.NewError(media.ClassInvalidLength, "invalid BER length").
				WithContainer("mxf").WithOffset(off + 16).WithTolerance(0)
		}
		payload := off + 16 + int64(lenBytes)
		if length < 0 || payload+length > w.size {
			// The fault is that the input ended, so report the first missing
			// byte rather than the KLV header that declared the overrun.
			return media.NewError(media.ClassTruncated,
				"KLV declares %d payload bytes but the input ends after %d", length, w.size-payload).
				WithContainer("mxf").WithOffset(w.size).WithTolerance(0)
		}
		switch {
		case key == ulHeaderPartition, key == ulBodyPartition, key == ulFooterPartition:
			w.sawHeader = w.sawHeader || key == ulHeaderPartition
		case key == ulMetadataSet:
			if err := w.readMetadata(payload, length); err != nil {
				return err
			}
		case key == ulIndexSegment:
			if err := w.readIndex(payload, length); err != nil {
				return err
			}
		case key == ulPrimer, key == ulRIP:
			// Known, no payload of interest.
		default:
			if track, ok := essenceTrack(key); ok {
				w.recordSample(track, payload, length, off)
			} else {
				w.unknown++
			}
		}
		off = payload + length
	}
	if !w.sawHeader {
		return media.NewError(media.ClassMalformed, "no header partition pack found").
			WithContainer("mxf").WithOffset(0)
	}
	return nil
}

// essenceTrack decodes the track number from a 16-byte essence key. The
// declared subset uses GC (picture) and AA (sound) element keys only.
func essenceTrack(key [16]byte) (uint32, bool) {
	var prefix [12]byte
	copy(prefix[:], key[:12])
	if prefix != ulEssenceGC && prefix != ulEssenceAA {
		return 0, false
	}
	return binary.BigEndian.Uint32(key[12:]), true
}

func (w *mxfWalker) readMetadata(off, length int64) error {
	buf := make([]byte, length)
	if _, err := w.src.ReadAt(buf, off); err != nil {
		return media.NewError(media.ClassTruncated, "metadata local set is truncated").
			WithContainer("mxf").WithOffset(off).WithTolerance(0).WithCause(err)
	}
	var cur *mxfTrackMeta
	for i := 0; i+4 <= len(buf); {
		tag := binary.BigEndian.Uint16(buf[i:])
		size := int(binary.BigEndian.Uint16(buf[i+2:]))
		i += 4
		if i+size > len(buf) {
			return media.NewError(media.ClassInvalidLength,
				"metadata item 0x%04x declares %d bytes past the local set", tag, size).
				WithContainer("mxf").WithOffset(off + int64(i-4)).WithTolerance(0)
		}
		val := buf[i : i+size]
		switch tag {
		case tagTrackNumber:
			if size != 4 {
				return media.NewError(media.ClassInvalidLength,
					"track number item has %d bytes, want 4", size).
					WithContainer("mxf").WithOffset(off + int64(i-4)).WithTolerance(0)
			}
			id := binary.BigEndian.Uint32(val)
			if id == 0 {
				return media.NewError(media.ClassMissingTrackID, "MXF track number is zero").
					WithContainer("mxf").WithOffset(off + int64(i-4))
			}
			if _, dup := w.byTrack[id]; dup {
				return media.NewError(media.ClassDuplicateTrackID,
					"MXF track number %d appears twice in the header metadata", id).
					WithContainer("mxf").WithTrack(id).WithOffset(off + int64(i-4))
			}
			cur = &mxfTrackMeta{number: id, rateN: 24, rateD: 1}
			w.byTrack[id] = cur
			w.metas = append(w.metas, cur)
		case tagEditRate:
			if cur == nil || size != 8 {
				return media.NewError(media.ClassInvalidLength,
					"edit rate item is malformed").WithContainer("mxf").
					WithOffset(off + int64(i-4)).WithTolerance(0)
			}
			cur.rateN = binary.BigEndian.Uint32(val)
			cur.rateD = binary.BigEndian.Uint32(val[4:])
			if cur.rateN == 0 || cur.rateD == 0 {
				return media.NewError(media.ClassInvalidLength, "edit rate has a zero component").
					WithContainer("mxf").WithTrack(cur.number).
					WithOffset(off + int64(i-4)).WithTolerance(0)
			}
		case tagEssenceType:
			if cur != nil && size == 1 {
				cur.audio = val[0] == 1
			}
		}
		i += size
	}
	return nil
}

func (w *mxfWalker) readIndex(off, length int64) error {
	buf := make([]byte, length)
	if _, err := w.src.ReadAt(buf, off); err != nil {
		return media.NewError(media.ClassTruncated, "index segment is truncated").
			WithContainer("mxf").WithOffset(off).WithTolerance(0).WithCause(err)
	}
	if len(buf) < 4 {
		return media.NewError(media.ClassInvalidLength, "index segment is too short").
			WithContainer("mxf").WithOffset(off).WithTolerance(0)
	}
	w.sawIndex = true
	count := int(binary.BigEndian.Uint32(buf))
	if 4+count*8 > len(buf) {
		return media.NewError(media.ClassInvalidLength,
			"index declares %d entries but the segment holds %d bytes", count, len(buf)).
			WithContainer("mxf").WithOffset(off).WithTolerance(0)
	}
	if len(w.metas) == 0 {
		return media.NewError(media.ClassMissingTrackID, "index segment precedes any track metadata").
			WithContainer("mxf").WithOffset(off)
	}
	// The generated subset writes one shared timestamp sequence per edit unit;
	// every track consumes the same values, unwrapped independently.
	for _, m := range w.metas {
		u := media.NewUnwrapper(32)
		var seq []int64
		for i := 0; i < count; i++ {
			raw := binary.BigEndian.Uint32(buf[4+i*8:])
			v, err := u.Unwrap(uint64(raw), uint64(off+int64(4+i*8)))
			if err != nil {
				return media.WithTrackContext(err, "mxf", m.number)
			}
			seq = append(seq, v)
		}
		w.indexTimes[m.number] = seq
	}
	return nil
}

func (w *mxfWalker) recordSample(track uint32, off, size, klvOff int64) {
	w.samples[track] = append(w.samples[track], mxfSample{offset: off, size: size, track: track})
}

func (w *mxfWalker) container() (*media.Container, error) {
	c := &media.Container{Format: "mxf", Path: w.src.Name(), Size: w.size, UnknownBoxes: w.unknown}
	for _, m := range w.metas {
		samples := w.samples[m.number]
		times, hasIndex := w.indexTimes[m.number]
		if hasIndex && len(times) != len(samples) {
			return nil, media.NewError(media.ClassMalformed,
				"index lists %d edit units but %d essence items were found", len(times), len(samples)).
				WithContainer("mxf").WithTrack(m.number)
		}
		idx := make(media.SliceIndex, len(samples))
		for i, s := range samples {
			ticks := int64(i)
			if hasIndex {
				ticks = times[i]
			}
			idx[i] = media.Sample{
				Seq:      uint32(i),
				DTS:      media.NewRat(ticks*int64(m.rateD), int64(m.rateN)),
				PTS:      media.NewRat(ticks*int64(m.rateD), int64(m.rateN)),
				Duration: media.NewRat(int64(m.rateD), int64(m.rateN)),
				Offset:   s.offset,
				Size:     s.size,
				Sync:     true,
			}
		}
		typ := media.MediaVideo
		if m.audio {
			typ = media.MediaAudio
		}
		t := &media.Track{
			ID:        media.TrackID{Container: "mxf", Value: m.number},
			Type:      typ,
			Codec:     mxfCodec(m.audio),
			Timescale: m.rateN,
			Index:     idx,
		}
		if len(idx) > 0 {
			last := idx[len(idx)-1]
			t.Duration = last.DTS.Add(last.Duration)
		}
		c.Tracks = append(c.Tracks, t)
	}
	if len(c.Tracks) == 0 {
		return nil, media.NewError(media.ClassMissingTrackID,
			"header metadata declares no tracks").WithContainer("mxf").WithOffset(0)
	}
	return c, nil
}

func mxfCodec(audio bool) string {
	if audio {
		return "pcm_s24le"
	}
	return "mxf-j2k"
}
