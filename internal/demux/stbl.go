package demux

import (
	"encoding/binary"
	"io"

	mp4 "github.com/abema/go-mp4"

	"mediapack/internal/clock"
	"mediapack/internal/fault"
)

// parseStbl reads the sample table boxes of one track.
func parseStbl(r io.ReaderAt, size int64, t *Track, in *tableInput, fl *fault.List) error {
	return walkBoxes(r, 0, size, func(bh boxHeader) error {
		switch bh.typ {
		case "stsd":
			p, err := readPayload(r, bh, maxMoovBytes)
			if err != nil {
				return err
			}
			t.Codec = parseStsdCodec(p)
		case "stts":
			var stts mp4.Stts
			if err := readInto(r, bh, &stts); err != nil {
				return err
			}
			for _, e := range stts.Entries {
				in.stts = append(in.stts, sttsEntry{Count: e.SampleCount, Delta: e.SampleDelta})
			}
		case "ctts":
			var ctts mp4.Ctts
			if err := readInto(r, bh, &ctts); err != nil {
				return err
			}
			for _, e := range ctts.Entries {
				in.ctts = append(in.ctts, cttsEntry{Count: e.SampleCount, Offset: e.SampleOffsetV1})
			}
		case "stsc":
			var stsc mp4.Stsc
			if err := readInto(r, bh, &stsc); err != nil {
				return err
			}
			for _, e := range stsc.Entries {
				in.stsc = append(in.stsc, stscEntry{
					FirstChunk: e.FirstChunk, SamplesPerChunk: e.SamplesPerChunk,
					DescIndex: e.SampleDescriptionIndex})
			}
		case "stsz":
			var stsz mp4.Stsz
			if err := readInto(r, bh, &stsz); err != nil {
				return err
			}
			if stsz.SampleSize != 0 { // constant size
				for i := uint32(0); i < stsz.SampleCount; i++ {
					in.stsz = append(in.stsz, stsz.SampleSize)
				}
			} else {
				in.stsz = append(in.stsz, stsz.EntrySize...)
			}
		case "stss":
			var stss mp4.Stss
			if err := readInto(r, bh, &stss); err != nil {
				return err
			}
			in.stss = map[uint32]bool{}
			for _, n := range stss.SampleNumber {
				in.stss[n] = true
			}
		case "stco":
			var stco mp4.Stco
			if err := readInto(r, bh, &stco); err != nil {
				return err
			}
			for _, o := range stco.ChunkOffset {
				in.stco = append(in.stco, uint64(o))
			}
		case "co64":
			var co64 mp4.Co64
			if err := readInto(r, bh, &co64); err != nil {
				return err
			}
			in.stco = append(in.stco, co64.ChunkOffset...)
		}
		return nil
	})
}

// parseStsdCodec extracts the first sample entry type (e.g. "avc1").
func parseStsdCodec(p []byte) string {
	// version/flags(4) + entry_count(4) + entry_size(4) + entry_type(4)
	if len(p) < 16 {
		return "unknown"
	}
	return string(p[12:16])
}

// recoverKeyframes rebuilds the random-access index of a video track whose
// stss box is missing by scanning AVC payloads for IDR access units.
// Intervals that cannot be confirmed are reported as untrusted.
func (c *Container) recoverKeyframes(file io.ReaderAt, t *Track) {
	t.IndexComplete = false
	if t.Codec != "avc1" {
		c.Faults.Add(fault.Recoverable, -1, t.ID, -1,
			"stss missing and codec %q not scannable; only first sample trusted", t.Codec)
		if len(t.Samples) > 0 {
			t.Samples[0].Keyframe = true
			c.Untrusted = append(c.Untrusted, Interval{
				StartTicks: 0,
				EndTicks:   clock.Ticks(t.Duration(), t.Timescale),
				Reason:     "keyframe index missing and not recoverable"})
		}
		return
	}
	recovered := 0
	for i := range t.Samples {
		s := &t.Samples[i]
		if !s.Available {
			continue
		}
		idr, err := scanAVCIDR(file, s.Offset, int64(s.Size))
		if err != nil {
			c.Faults.Add(fault.Recoverable, s.Offset, t.ID,
				clock.Ticks(s.DTS, t.Timescale), "avc scan: %v", err)
			continue
		}
		if idr {
			s.Keyframe = true
			recovered++
		}
	}
	c.Faults.Add(fault.Recoverable, -1, t.ID, -1,
		"stss missing; recovered %d keyframes by AVC payload scan", recovered)
}

// scanAVCIDR reports whether the AVCC sample at off contains an IDR NAL.
func scanAVCIDR(r io.ReaderAt, off, size int64) (bool, error) {
	buf := make([]byte, size)
	if _, err := r.ReadAt(buf, off); err != nil {
		return false, err
	}
	for len(buf) >= 4 {
		n := binary.BigEndian.Uint32(buf[:4])
		buf = buf[4:]
		if n == 0 || int64(n) > int64(len(buf)) {
			return false, nil
		}
		if buf[0]&0x1f == 5 { // NAL type 5 = IDR slice
			return true, nil
		}
		buf = buf[n:]
	}
	return false, nil
}
